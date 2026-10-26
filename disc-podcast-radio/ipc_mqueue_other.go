//go:build !linux || !mipsle

package main

import (
	"fmt"
	"os"
)

func sendPOSIXMessageQueue(queueName string, msg string) error {
	return fmt.Errorf("posix mqueue 不支持当前运行平台 (仅在 linux/mipsle 固件中有效)")
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
