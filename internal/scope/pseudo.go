package scope

// pseudoFSTypes are kernel-provided filesystems that never represent real,
// countable disk usage. They are skipped during descent when ExcludeVirtual is
// on, regardless of path. tmpfs/ramfs/fuse are deliberately NOT here: those
// may hold user data a person would reasonably want measured.
var pseudoFSTypes = map[string]bool{
	"proc":        true,
	"sysfs":       true,
	"cgroup":      true,
	"cgroup2":     true,
	"devtmpfs":    true,
	"devpts":      true,
	"debugfs":     true,
	"tracefs":     true,
	"configfs":    true,
	"securityfs":  true,
	"pstore":      true,
	"selinuxfs":   true,
	"mqueue":      true,
	"autofs":      true,
	"rpc_pipefs":  true,
	"binfmt_misc": true,
	"hugetlbfs":   true,
	"bpf":         true,
}

// isPseudoFSType reports whether a resolved fstype is a kernel pseudo-fs.
func isPseudoFSType(fs string) bool {
	return pseudoFSTypes[fs]
}
