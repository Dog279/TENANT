package procprotect_test

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// endTreeWithThisProcess puts this process in a job that Windows kills when
// its last handle closes, as Tenant itself is (TEN-401). Windows does not end
// a process's children with it: without the job a browser this test started
// would outlive a test that crashed or was killed. The handle is never
// closed, so the job ends when this process does, however that happens.
func endTreeWithThisProcess(t *testing.T) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatalf("SetInformationJobObject: %v", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		t.Fatalf("AssignProcessToJobObject: %v", err)
	}
}
