package main

import (
	"flag"
	"time"

	"gogs.io/gogs/internal/ssh"
)

func main() {
	var port *int
	port = flag.Int("p", 9393, "ssh server port")
	flag.Parse()

	ssh.Listen(
		"0.0.0.0",
		*port,
		[]string{"aes128-ctr", "aes192-ctr", "aes256-ctr", "aes128-gcm@openssh.com", "arcfour256", "arcfour128"},
		[]string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256", "hmac-sha1"})

	for {
		time.Sleep(time.Second)
	}
}
