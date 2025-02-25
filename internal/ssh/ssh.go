// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package ssh

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/unknwon/com"
	"golang.org/x/crypto/ssh"
	log "unknwon.dev/clog/v2"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/db"
)

func cleanCommand(cmd string) string {
	i := strings.Index(cmd, "git")
	if i == -1 {
		return cmd
	}
	return cmd[i:]
}

func handleServerConn(keyID string, chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Error("Error accepting channel: %v", err)
			continue
		}

		go func(in <-chan *ssh.Request) {
			defer func() {
				_ = ch.Close()
			}()
			for req := range in {
				payload := cleanCommand(string(req.Payload))
				switch req.Type {
				case "env":
					var env struct {
						Name  string
						Value string
					}
					if err := ssh.Unmarshal(req.Payload, &env); err != nil {
						log.Warn("SSH: Invalid env payload %q: %v", req.Payload, err)
						continue
					}
					// Sometimes the client could send malformed command (i.e. missing "="),
					// see https://discuss.gogs.io/t/ssh/3106.
					if env.Name == "" || env.Value == "" {
						log.Warn("SSH: Invalid env arguments: %+v", env)
						continue
					}

					_, stderr, err := com.ExecCmd("env", fmt.Sprintf("%s=%s", env.Name, env.Value))
					if err != nil {
						log.Error("env: %v - %s", err, stderr)
						return
					}

				case "exec":
					cmdName := strings.TrimLeft(payload, "'()")
					log.Info("SSH: Payload: %v", cmdName)

					args := []string{"serv", "key-" + keyID, "--config=" + conf.CustomConf}
					log.Info("SSH: Arguments: %v", args)
					// cmd := exec.Command(conf.AppPath(), args...)

					cmdPartsTemp := strings.Split(cmdName, " ")
					var cmdParts []string
					for i := range cmdParts {
						cmdPartsTemp[i] = strings.TrimSpace(cmdPartsTemp[i])
						cmdPartsTemp[i] = fmt.Sprint(cmdPartsTemp[i])
						cmdPartsTemp[i] = strings.Map(func(r rune) rune {
							if unicode.IsPrint(r) {
								return r
							}
							return -1
						}, cmdPartsTemp[i])
						if len(cmdPartsTemp[i]) > 0 {
							cmdParts = append(cmdParts, cmdPartsTemp[i])
							log.Trace("SSH: arg[%d] length %d, %s", i, len(cmdParts[i]), cmdParts[i])
						}
					}

					if cmdParts[0] == "cat" {
						filePath := cmdParts[1]
						f, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
						if err != nil {
							log.Error("SSH: open error: %v", err)
							return
						}
						_, _ = io.Copy(f, ch)
						f.Close()
					} else {
						if len(cmdParts) > 0 {
							cmdParts[0], err = exec.LookPath(cmdParts[0])
							if err != nil {
								log.Error("SSH: cannot find %d: %v", cmdParts[0], err)
								return
							}
						}
						var cmd *exec.Cmd
						if len(cmdParts) > 1 {
							cmd = exec.Command(cmdParts[0], cmdParts[1:]...)
						} else if len(cmdParts) == 1 {
							cmd = exec.Command(cmdParts[0])
						} else {
							return
						}
						// cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmdName)

						stdout, err := cmd.StdoutPipe()
						if err != nil {
							log.Error("SSH: StdoutPipe: %v", err)
							return
						}
						stderr, err := cmd.StderrPipe()
						if err != nil {
							log.Error("SSH: StderrPipe: %v", err)
							return
						}
						input, err := cmd.StdinPipe()
						if err != nil {
							log.Error("SSH: StdinPipe: %v", err)
							return
						}
						u, err := user.Current()
						if err != nil {
							log.Error("SSH: ERROR: %v", err)
							return
						}
						uid, err := strconv.Atoi(u.Uid)
						if err != nil {
							log.Error("SSH: ERROR: %v", err)
							return
						}
						gid, err := strconv.Atoi(u.Gid)
						if err != nil {
							log.Error("SSH: ERROR: %v", err)
							return
						}
						cmd.SysProcAttr = &syscall.SysProcAttr{}
						cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}

						// FIXME: check timeout
						log.Info("cmd: %s", cmd.String())
						if err = cmd.Start(); err != nil {
							log.Error("SSH: Start: %v", err)
							return
						}

						_ = req.Reply(true, nil)
						go func() {
							_, _ = io.Copy(input, ch)
						}()
						_, _ = io.Copy(ch, stdout)
						_, _ = io.Copy(ch.Stderr(), stderr)

						if err = cmd.Wait(); err != nil {
							log.Error("SSH: Wait: %v", err)
							return
						}
					}

					_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				default:
				}
			}
		}(reqs)
	}
}

func listen(config *ssh.ServerConfig, host string, port int) {
	listener, err := net.Listen("tcp", host+":"+com.ToStr(port))
	if err != nil {
		log.Fatal("Failed to start SSH server: %v", err)
	}
	for {
		// Once a ServerConfig has been configured, connections can be accepted.
		conn, err := listener.Accept()
		if err != nil {
			log.Error("SSH: Error accepting incoming connection: %v", err)
			continue
		}

		// Before use, a handshake must be performed on the incoming net.Conn.
		// It must be handled in a separate goroutine,
		// otherwise one user could easily block entire loop.
		// For example, user could be asked to trust server key fingerprint and hangs.
		go func() {
			log.Trace("SSH: Handshaking for %s", conn.RemoteAddr())
			sConn, chans, reqs, err := ssh.NewServerConn(conn, config)
			if err != nil {
				if err == io.EOF {
					log.Warn("SSH: Handshaking was terminated: %v", err)
				} else {
					log.Error("SSH: Error on handshaking: %v", err)
				}
				return
			}

			log.Trace("SSH: Connection from %s (%s)", sConn.RemoteAddr(), sConn.ClientVersion())
			// The incoming Request channel must be serviced.
			go ssh.DiscardRequests(reqs)
			go handleServerConn(sConn.Permissions.Extensions["key-id"], chans)
		}()
	}
}

// Listen starts a SSH server listens on given port.
func Listen(host string, port int, ciphers, macs []string) {
	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: ciphers,
			MACs:    macs,
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			pkey, err := db.SearchPublicKeyByContent(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
			if err != nil {
				log.Error("SearchPublicKeyByContent: %v", err)
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{"key-id": com.ToStr(pkey.ID)}}, nil
		},
	}

	keyPath := filepath.Join(conf.Server.AppDataPath, "ssh", fmt.Sprintf("gogs_%d.rsa", port))
	if !com.IsExist(keyPath) {
		if err := os.MkdirAll(filepath.Dir(keyPath), os.ModePerm); err != nil {
			panic(err)
		}
		path, _ := exec.LookPath("ssh-keygen")
		_, stderr, err := com.ExecCmd(path, "-f", keyPath, "-t", "rsa", "-m", "PEM", "-N", "")
		if err != nil {
			panic(fmt.Sprintf("Failed to generate private key: %v - %s", err, stderr))
		}
		log.Trace("SSH: New private key is generateed: %s", keyPath)
	}

	privateBytes, err := ioutil.ReadFile(keyPath)
	if err != nil {
		panic("SSH: Failed to load private key: " + err.Error())
	}
	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		panic("SSH: Failed to parse private key: " + err.Error())
	}
	config.AddHostKey(private)

	go listen(config, host, port)
}
