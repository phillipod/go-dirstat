//go:build darwin

package fsinfo

import "syscall"

func volumeIdentityFor(path string) (mountPoint, filesystem, device string) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", "", ""
	}
	return statfsString(stat.Mntonname[:]), statfsString(stat.Fstypename[:]), statfsString(stat.Mntfromname[:])
}

func statfsString(value []int8) string {
	result := make([]byte, 0, len(value))
	for _, character := range value {
		if character == 0 {
			break
		}
		result = append(result, byte(character))
	}
	return string(result)
}
