//go:build linux && mipsle

package main

import (
	"os"
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

// sendPOSIXMessageQueue is a safe no-op to prevent corrupting mq_player message queue.
func sendPOSIXMessageQueue(queueName string, msg string) error {
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
