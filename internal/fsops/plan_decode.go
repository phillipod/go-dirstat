package fsops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// ReadPlanLimited buffers at most maxBytes+1 bytes before parsing. Reading the
// sentinel byte first prevents a syntactically valid prefix from being applied
// when the rest of an oversized plan was hidden by a limiting reader.
func ReadPlanLimited(r io.Reader, maxBytes int64) (Plan, error) {
	if maxBytes == math.MaxInt64 {
		return ReadPlan(r)
	}
	data, err := readLimitedPlanBytes(r, maxBytes)
	if err != nil {
		return Plan{}, err
	}
	return ReadPlan(bytes.NewReader(data))
}

// ReadOperationRequestsLimited reads strict request-only JSONL for batch plan
// creation. Each physical line is one OperationRequest; headers, IDs and
// metadata guards are not accepted on this surface.
func ReadOperationRequestsLimited(r io.Reader, maxBytes int64) ([]OperationRequest, error) {
	data, err := readLimitedPlanBytes(r, maxBytes)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	requests := make([]OperationRequest, 0)
	for line := 1; ; line++ {
		record, found, readErr := readPlanRecord(reader, line)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			if len(requests) == 0 {
				return nil, errors.New("operation request input is empty")
			}
			return requests, nil
		}
		if !utf8.Valid(record) {
			return nil, fmt.Errorf("operation request on line %d is not valid UTF-8", line)
		}
		var request OperationRequest
		if err := decodePlanRecord(record, &request); err != nil {
			return nil, fmt.Errorf("read operation request on line %d: %w", line, err)
		}
		requests = append(requests, request)
		if len(requests) > MaxPlanOperations {
			return nil, fmt.Errorf("operation request input exceeds maximum of %d operations", MaxPlanOperations)
		}
	}
}

func readLimitedPlanBytes(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, errors.New("plan size limit must not be negative")
	}
	if maxBytes == math.MaxInt64 {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read plan bytes: %w", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read plan bytes: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("plan exceeds maximum size of %d bytes", maxBytes)
	}
	return data, nil
}

func readPlanJSONL(r io.Reader) (Plan, error) {
	reader := bufio.NewReader(r)
	headerRecord, ok, err := readPlanRecord(reader, 1)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		return Plan{}, errors.New("read plan header: plan is empty")
	}
	var plan Plan
	if err := decodePlanRecord(headerRecord, &plan.Header); err != nil {
		return Plan{}, fmt.Errorf("read plan header on line 1: %w", err)
	}
	if plan.Header.Type != planRecordType || !supportedVersion(plan.Header.Version) {
		return Plan{}, fmt.Errorf("unsupported plan header type=%q version=%d", plan.Header.Type, plan.Header.Version)
	}

	seenIDs := make(map[string]bool)
	for line := 2; ; line++ {
		record, found, readErr := readPlanRecord(reader, line)
		if readErr != nil {
			return Plan{}, readErr
		}
		if !found {
			return plan, nil
		}
		var operation Operation
		if err := decodePlanRecord(record, &operation); err != nil {
			return Plan{}, fmt.Errorf("read operation on line %d: %w", line, err)
		}
		if operation.Type != operationRecordType {
			return Plan{}, fmt.Errorf("unexpected plan record type %q on line %d", operation.Type, line)
		}
		if strings.TrimSpace(operation.ID) == "" {
			return Plan{}, fmt.Errorf("operation ID is required on line %d", line)
		}
		if seenIDs[operation.ID] {
			return Plan{}, fmt.Errorf("duplicate operation ID %q on line %d", operation.ID, line)
		}
		seenIDs[operation.ID] = true
		plan.Operations = append(plan.Operations, operation)
	}
}

func readPlanRecord(reader *bufio.Reader, line int) ([]byte, bool, error) {
	record, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("read plan line %d: %w", line, err)
	}
	if len(record) == 0 && errors.Is(err, io.EOF) {
		return nil, false, nil
	}
	record = bytes.TrimSuffix(record, []byte{'\n'})
	record = bytes.TrimSuffix(record, []byte{'\r'})
	if len(bytes.TrimSpace(record)) == 0 {
		return nil, false, fmt.Errorf("read plan line %d: blank JSONL records are not allowed", line)
	}
	return record, true, nil
}

func decodePlanRecord(record []byte, destination any) error {
	if err := validatePlanJSONRecord(record); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value in record")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func validatePlanJSONRecord(record []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON object: %w", err)
	}
	if token != json.Delim('{') {
		return errors.New("JSONL record must be one object")
	}
	if err := consumePlanJSONObject(decoder, ""); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value in record")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func consumePlanJSONObject(decoder *json.Decoder, path string) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode object field: %w", err)
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("object field name is not a string")
		}
		fieldPath := field
		if path != "" {
			fieldPath = path + "." + field
		}
		if seen[field] {
			return fmt.Errorf("duplicate field %q", fieldPath)
		}
		seen[field] = true
		if err := consumePlanJSONValue(decoder, fieldPath); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("close JSON object: %w", err)
	}
	if token != json.Delim('}') {
		return errors.New("malformed JSON object")
	}
	return nil
}

func consumePlanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode field %q: %w", path, err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return consumePlanJSONObject(decoder, path)
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := consumePlanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil {
			return fmt.Errorf("close array %q: %w", path, err)
		} else if token != json.Delim(']') {
			return fmt.Errorf("field %q has a malformed array", path)
		}
		return nil
	default:
		return fmt.Errorf("field %q has unexpected delimiter %q", path, delimiter)
	}
}
