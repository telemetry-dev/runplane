// Package guest is the contract between runplane and runplane-init in a
// sandbox: the folders and drives of the guest, the job config, the control
// channel protocol, and the runner patch. runplane-init is a static guest
// binary, so this package imports no heavy packages.
package guest

// The control channel in container and dev mode. The driver mounts a host
// folder at CtlDir, with the Unix socket SocketFile, or in dev mode the file
// AddrFile.
const (
	CtlDir     = "/run/runplane"
	SocketFile = "control.sock"
)

// VsockPort is the vsock port on the host (CID 2) that the guest dials in VM
// mode.
const VsockPort = 1024

// ScratchDir is the scratch disk in the guest. It is erased after the job.
const ScratchDir = "/mnt/scratch"

// Folders on the scratch disk.
const (
	ScratchRunner = "runner" // upper layer of the runner folder
	// ScratchRoot has the upper layers of the root in VM mode, and of the
	// image folders that jobs write in container mode.
	ScratchRoot   = "root"
	ScratchDocker = "docker" // at DockerDir
	// ScratchTmp is at /tmp in container mode. A VM has /tmp on scratch
	// through the root overlay.
	ScratchTmp = "tmp"
)

// DockerDir is the data folder of dockerd. It is on the scratch disk, because
// overlay2 does not work on top of an overlay.
const DockerDir = "/var/lib/docker"

// Drive is a disk of a VM.
type Drive int

// The host attaches the drives in this order.
const (
	DriveRoot    Drive = iota // read-only root disk of the image
	DriveRunner               // read-only runner bundle
	DriveScratch              // sparse ext4 at ScratchDir
)

// Device is the device of the drive in the guest. The guest kernel names the
// virtio disks vda, vdb, and so on, in the order that the host attaches them.
func (d Drive) Device() string { return "/dev/vd" + string(rune('a'+d)) }

// InitPath is runplane-init on the root disk of a VM.
const InitPath = "/sbin/runplane-init"

// VMFlag on the kernel command line tells runplane-init that it is PID 1 of
// a VM.
const VMFlag = "runplane.mode=vm"

// KernelArgs are the kernel options of a VM that runplane-init needs. With
// reboot=k, the reboot of init ends Firecracker on x86, which has no
// power-off device. With panic=1, the exit of init resets the VM.
const KernelArgs = "reboot=k panic=1 init=" + InitPath + " " + VMFlag

// MountPoints are the folders that runplane-init mounts file systems on in a
// VM, with the mode that the root disk gives them. The root disk is
// read-only, so they must exist in it.
var MountPoints = []MountPoint{
	{"/dev", 0o755},
	{"/proc", 0o555},
	{"/sys", 0o555},
	{"/run", 0o755},
	{"/tmp", 0o1777},
	{ScratchDir, 0o755},
}

type MountPoint struct {
	Path string
	Mode int64 // the Unix mode bits, as in a tar header
}

// The user that runs the runner and the debug shells. The image makes it,
// with the UID of the GitHub-hosted runners, and makes RunnerHome for it.
const (
	RunnerUser = "runner"
	RunnerUID  = 1001
	// RunnerHome is the folder of the runner: an overlay of the runner
	// bundle, so that the runner can write its config, _diag, and _work.
	RunnerHome = "/home/runner/actions-runner"
)

// LabelManaged=true marks the Docker objects that runplane makes. The builtin
// images have it in their Dockerfile, so the images FROM them have it too.
const LabelManaged = "dev.runplane.managed"
