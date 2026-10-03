package storage

import (
	"errors"
	"fmt"
)

// CommandError 是存储栈所有外部命令（zfs、zpool、targetcli、qemu-img 等）失败时统一的错误形态，带程序名、参数和输出。
type CommandError struct {
	Name   string
	Args   []string
	Output string
	Err    error
}

func (e CommandError) Error() string {
	return fmt.Sprintf("%s %v failed: %v: %s", e.Name, e.Args, e.Err, e.Output)
}

func (e CommandError) Unwrap() error {
	return e.Err
}

func IsCommandError(err error) bool {
	var cmdErr CommandError
	return errors.As(err, &cmdErr)
}
