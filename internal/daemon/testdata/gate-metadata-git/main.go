package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"time"
)

func main() {
	cmd := exec.Command(os.Getenv("NM160_REAL_GIT"), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	if address := os.Getenv("NM160_WRITE_BARRIER"); address != "" && slices.Contains(os.Args[1:], "receive.advertisePushOptions") && slices.Contains(os.Args[1:], "true") {
		conn, err := net.DialTimeout("tcp", address, 10*time.Second)
		if err == nil {
			err = conn.SetDeadline(time.Now().Add(20 * time.Second))
			if err == nil {
				_, err = conn.Write([]byte{1})
			}
			if err == nil {
				_, err = io.ReadFull(conn, make([]byte, 1))
			}
			err = errors.Join(err, conn.Close())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
	}
}
