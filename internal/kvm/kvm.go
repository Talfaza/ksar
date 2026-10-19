package kvm

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// ioctl calls ioctl(2) with an integer argument.
func ioctl(fd int, req, arg uintptr) (uintptr, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if errno != 0 {
		return r, errno
	}
	return r, nil
}

// ioctlPtr calls ioctl(2) with a pointer argument. The uintptr conversion
// happens inside the Syscall argument list, which keeps p alive and pinned
// for the duration of the call as unsafe.Pointer rules require.
func ioctlPtr(fd int, req uintptr, p unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(p))
	if errno != 0 {
		return errno
	}
	return nil
}

// System is an open handle to /dev/kvm.
type System struct {
	fd           int
	vcpuMmapSize int
}

// Open opens /dev/kvm and checks the API version.
func Open() (*System, error) {
	fd, err := syscall.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/kvm: %w (is KVM enabled and is your user in the kvm group?)", err)
	}
	v, err := ioctl(fd, kvmGetAPIVersion, 0)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("KVM_GET_API_VERSION: %w", err)
	}
	if v != APIVersion {
		syscall.Close(fd)
		return nil, fmt.Errorf("unsupported KVM API version %d (want %d)", v, APIVersion)
	}
	size, err := ioctl(fd, kvmGetVCPUMmapSize, 0)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("KVM_GET_VCPU_MMAP_SIZE: %w", err)
	}
	if int(size) < runUnionOffset+256 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vcpu mmap size %d is too small", size)
	}
	return &System{fd: fd, vcpuMmapSize: int(size)}, nil
}

// Close closes /dev/kvm. VMs created from it stay valid.
func (s *System) Close() error { return syscall.Close(s.fd) }

// CreateVM creates a new, empty virtual machine.
func (s *System) CreateVM() (*VM, error) {
	// Arg 0 = default machine type.
	fd, err := ioctl(s.fd, kvmCreateVM, 0)
	if err != nil {
		return nil, fmt.Errorf("KVM_CREATE_VM: %w", err)
	}
	syscall.CloseOnExec(int(fd))
	return &VM{fd: int(fd), sys: s}, nil
}

// VM is a KVM virtual machine: guest memory plus vCPUs.
type VM struct {
	fd    int
	sys   *System
	mem   [][]byte
	vcpus []*VCPU
}

// AddMemory allocates size bytes of anonymous host memory and maps it into
// the guest at guestPhys. It returns the host view of that memory.
// size and guestPhys must be page aligned.
func (vm *VM) AddMemory(slot uint32, guestPhys uint64, size int) ([]byte, error) {
	const page = 4096
	if size <= 0 || size%page != 0 || guestPhys%page != 0 {
		return nil, fmt.Errorf("memory region must be page aligned (gpa=%#x size=%#x)", guestPhys, size)
	}
	mem, err := syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_NORESERVE)
	if err != nil {
		return nil, fmt.Errorf("mmap guest memory: %w", err)
	}
	region := userspaceMemoryRegion{
		Slot:          slot,
		GuestPhysAddr: guestPhys,
		MemorySize:    uint64(size),
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	if err := ioctlPtr(vm.fd, kvmSetUserMemoryRegion, unsafe.Pointer(&region)); err != nil {
		syscall.Munmap(mem)
		return nil, fmt.Errorf("KVM_SET_USER_MEMORY_REGION: %w", err)
	}
	vm.mem = append(vm.mem, mem)
	return mem, nil
}

// CreateVCPU creates a vCPU and maps its shared kvm_run page.
func (vm *VM) CreateVCPU(id int) (*VCPU, error) {
	fd, err := ioctl(vm.fd, kvmCreateVCPU, uintptr(id))
	if err != nil {
		return nil, fmt.Errorf("KVM_CREATE_VCPU: %w", err)
	}
	syscall.CloseOnExec(int(fd))
	run, err := syscall.Mmap(int(fd), 0, vm.sys.vcpuMmapSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(int(fd))
		return nil, fmt.Errorf("mmap kvm_run: %w", err)
	}
	c := &VCPU{ID: id, fd: int(fd), run: run}
	vm.vcpus = append(vm.vcpus, c)
	return c, nil
}

// Close destroys the VM, its vCPUs and its memory.
func (vm *VM) Close() error {
	var errs []error
	for _, c := range vm.vcpus {
		errs = append(errs, c.close())
	}
	errs = append(errs, syscall.Close(vm.fd))
	for _, m := range vm.mem {
		errs = append(errs, syscall.Munmap(m))
	}
	vm.vcpus, vm.mem = nil, nil
	return errors.Join(errs...)
}

// VCPU is one virtual CPU. All calls to Run must come from a single
// goroutine locked to its OS thread (runtime.LockOSThread).
type VCPU struct {
	ID  int
	fd  int
	run []byte
}

func (c *VCPU) close() error {
	return errors.Join(syscall.Munmap(c.run), syscall.Close(c.fd))
}

// Run enters the guest until the next exit. It returns syscall.EINTR when
// a signal or ImmediateExit interrupted it; the caller decides whether to
// re-enter.
func (c *VCPU) Run() error {
	_, err := ioctl(c.fd, kvmRun, 0)
	return err
}

// RunState returns the shared kvm_run header.
func (c *VCPU) RunState() *Run { return (*Run)(unsafe.Pointer(&c.run[0])) }

// IO returns the details of a KVM_EXIT_IO exit.
func (c *VCPU) IO() *ExitIODetails {
	return (*ExitIODetails)(unsafe.Pointer(&c.run[runUnionOffset]))
}

// IOData returns the data buffer of a KVM_EXIT_IO exit. For OUT it holds what
// the guest wrote; for IN the VMM fills it. The offset comes from the kernel,
// but it is still bounds-checked before use.
func (c *VCPU) IOData() ([]byte, error) {
	d := c.IO()
	n := uint64(d.Size) * uint64(d.Count)
	if d.DataOffset > uint64(len(c.run)) || n > uint64(len(c.run))-d.DataOffset {
		return nil, fmt.Errorf("io data out of range (offset=%d len=%d)", d.DataOffset, n)
	}
	return c.run[d.DataOffset : d.DataOffset+n], nil
}

// FailEntry returns the details of a KVM_EXIT_FAIL_ENTRY exit.
func (c *VCPU) FailEntry() *ExitFailEntryDetails {
	return (*ExitFailEntryDetails)(unsafe.Pointer(&c.run[runUnionOffset]))
}

// InternalError returns the details of a KVM_EXIT_INTERNAL_ERROR exit.
func (c *VCPU) InternalError() *ExitInternalErrorDetails {
	return (*ExitInternalErrorDetails)(unsafe.Pointer(&c.run[runUnionOffset]))
}

// GetRegs reads the general-purpose registers.
func (c *VCPU) GetRegs() (Regs, error) {
	var r Regs
	err := ioctlPtr(c.fd, kvmGetRegs, unsafe.Pointer(&r))
	return r, wrap("KVM_GET_REGS", err)
}

// SetRegs writes the general-purpose registers.
func (c *VCPU) SetRegs(r Regs) error {
	return wrap("KVM_SET_REGS", ioctlPtr(c.fd, kvmSetRegs, unsafe.Pointer(&r)))
}

// GetSregs reads the segment and control registers.
func (c *VCPU) GetSregs() (Sregs, error) {
	var s Sregs
	err := ioctlPtr(c.fd, kvmGetSregs, unsafe.Pointer(&s))
	return s, wrap("KVM_GET_SREGS", err)
}

// SetSregs writes the segment and control registers.
func (c *VCPU) SetSregs(s Sregs) error {
	return wrap("KVM_SET_SREGS", ioctlPtr(c.fd, kvmSetSregs, unsafe.Pointer(&s)))
}

func wrap(op string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}
