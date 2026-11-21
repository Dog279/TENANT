package procprotect

import "os/exec"

// Start, Run, Output and CombinedOutput are cmd's own methods of those names
// for a process Tenant starts: the child is an ordinary process of the user
// (Child), and the pipes to it are made where they can be opened (Ordinary).
// Every command Tenant starts goes through one of them, or through Child and
// Ordinary itself; on Windows a child started any other way fails to start
// or cannot make a pipe of its own (spawn_windows.go). Elsewhere they are
// cmd's methods and nothing more.

func Start(cmd *exec.Cmd) (err error) {
	Child(cmd)
	Ordinary(func() { err = cmd.Start() })
	return err
}

func Run(cmd *exec.Cmd) (err error) {
	Child(cmd)
	Ordinary(func() { err = cmd.Run() })
	return err
}

func Output(cmd *exec.Cmd) (out []byte, err error) {
	Child(cmd)
	Ordinary(func() { out, err = cmd.Output() })
	return out, err
}

func CombinedOutput(cmd *exec.Cmd) (out []byte, err error) {
	Child(cmd)
	Ordinary(func() { out, err = cmd.CombinedOutput() })
	return out, err
}
