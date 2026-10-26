//go:build linux && mipsle

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Linux MIPS32 O32 POSIX message queue syscall constants.
// On MIPS32 O32:
// __NR_Linux = 4000
// __NR_mq_open = 4271 (0x10af)
// __NR_mq_timedsend = 4273 (0x10b1)
const (
	sysMqOpen      = 4271
	sysMqTimedsend = 4273

	// Linux MIPS open flags:
	// O_WRONLY = 0x0001
	// O_RDWR   = 0x0002
	// O_NONBLOCK = 0x0080
	mipsOWronly   = 0x0001
	mipsORdwr     = 0x0002
	mipsONonblock = 0x0080
)

// sendPOSIXMessageQueue sends msg to the POSIX message queue named queueName (e.g. "/player").
func sendPOSIXMessageQueue(queueName string, msg string) error {
	nameBytes, err := syscall.BytePtrFromString(queueName)
	if err != nil {
		return err
	}

	// Try O_WRONLY | O_NONBLOCK first
	flags := uintptr(mipsOWronly | mipsONonblock)
	mqdes, _, errno := syscall.Syscall6(sysMqOpen, uintptr(unsafe.Pointer(nameBytes)), flags, 0, 0, 0, 0)
	if errno != 0 || int(mqdes) < 0 {
		// Try O_RDWR | O_NONBLOCK as fallback
		flags = uintptr(mipsORdwr | mipsONonblock)
		mqdes, _, errno = syscall.Syscall6(sysMqOpen, uintptr(unsafe.Pointer(nameBytes)), flags, 0, 0, 0, 0)
		if errno != 0 || int(mqdes) < 0 {
			return fmt.Errorf("mq_open(%s) 失败: errno=%d", queueName, errno)
		}
	}
	defer syscall.Close(int(mqdes))

	msgBytes := []byte(msg)
	msgPtr := uintptr(unsafe.Pointer(&msgBytes[0]))
	msgLen := uintptr(len(msgBytes))

	// __NR_mq_timedsend(mqdes, msg_ptr, msg_len, prio=0, abs_timeout=0)
	_, _, errno = syscall.Syscall6(sysMqTimedsend, mqdes, msgPtr, msgLen, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("mq_timedsend(%s) 失败: errno=%d", queueName, errno)
	}
	return nil
}

func checkMQueuePlayer() SocketInfo {
	path := "/dev/mqueue/player"
	info := SocketInfo{Path: path}
	st, err := os.Stat(path)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Exists = true
	info.Mode = st.Mode().String()
	info.Size = st.Size()
	info.ModTime = st.ModTime().Format("2006-01-02 15:04:05")
	return info
}
