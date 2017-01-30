package sshgit

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/gogits/gogs/modules/log"
)

var packageName = "SSHGit"

func FormatLog(s string) string {
	return fmt.Sprintf("%s: %s", packageName, s)
}

type SSHKeygenConfig struct {
	// Default to rsa
	Type string
	// Default to no password (empty string)
	Passphrase string
}

type ServerConfig struct {
	// Default to localhost
	Host              string
	Port              uint
	PrivatekeyPath    string
	PublicKeyCallback func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error)
	KeygenConfig      SSHKeygenConfig
}

// Starts an SSH server on given port
func Listen(config ServerConfig) {
	if config.PublicKeyCallback == nil {
		log.Fatal(4, FormatLog("PublicKeyCallback cannot be nil"))
	}
	if config.PrivatekeyPath == "" {
		log.Fatal(4, FormatLog("PrivatekeyPath cannot be empty"))
	}
	if config.KeygenConfig.Type == "" {
		config.KeygenConfig.Passphrase = "rsa"
	}

	sshConfig := &ssh.ServerConfig{PublicKeyCallback: config.PublicKeyCallback}
	keyPath := config.PrivatekeyPath
	if !FileExists(keyPath) {
		os.MkdirAll(filepath.Dir(keyPath), os.ModePerm)

		// Generate a new ssh key pair without password
		// -f <filename>
		// -t <keytype>
		// -N <new_passphrase>
		_, stderr, err := ExecCmd("ssh-keygen", "-f", keyPath, "-t", config.KeygenConfig.Type, "-N", config.KeygenConfig.Passphrase)
		if err != nil {
			log.Fatal(4, FormatLog("Failed to generate private key: %v - %s"), err, stderr)
		}
		log.Trace(FormatLog("Generated a new private key at: %s"), keyPath)
	}

	// Read private key
	privateBytes, err := ioutil.ReadFile(keyPath)
	if err != nil {
		log.Fatal(4, FormatLog("Failed to read private key"))
	}
	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal(4, FormatLog("Failed to parse private key"))
	}
	sshConfig.AddHostKey(private)

	host := config.Host
	if host == "" {
		host = "localhost"
	}

	go serve(sshConfig, host, config.Port)
}

// Actual server
func serve(config *ssh.ServerConfig, host string, port uint) {
	// Listen on given host and port
	listener, err := net.Listen("tcp", host+":"+UIntToStr(port))
	if err != nil {
		log.Fatal(4, FormatLog("Failed to start SSH server: %v"), err)
	}

	// Infinite loop
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Error(3, FormatLog("Error accepting incoming connection: %v"), err)
			continue
		}

		// Before use, a handshake must be performed on the incoming
		// net.Conn.
		// It must be handled in a separate goroutine, otherwise one
		// user could easily block entire loop. For example, user could
		// be asked to trust server key fingerprint and hangs.
		go func() {
			log.Warn(FormatLog("Handshaking was terminated: %v"), err)
			sConn, channels, reqs, err := ssh.NewServerConn(conn, config)
			if err != nil {
				if err == io.EOF {
					log.Warn(FormatLog(fmt.Sprintf("Handshaking was terminated: %v", err)))
				} else {
					log.Error(3, FormatLog(fmt.Sprintf("Error on handshaking: %v", err)))
				}
				return
			}

			log.Trace(FormatLog(fmt.Sprintf("Connection from %s (%s)", sConn.RemoteAddr(), sConn.ClientVersion())))
			go ssh.DiscardRequests(reqs)
			go handleServerConn(sConn.Permissions.Extensions["key-id"], channels)
		}()
	}
}

// Remove unwanted characters in the received command
func cleanCommand(cmd string) string {
	i := strings.Index(cmd, "git")
	if i == -1 {
		return cmd
	}
	return cmd[i:]
}

func handleServerConn(keyID string, chans <-chan ssh.NewChannel) {
	fmt.Println("Handle server connection")

	// Loop on channels
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Error(3, FormatLog("Error accepting channel: %v"), err)
			continue
		}

		go func(in <-chan *ssh.Request) {
			defer ch.Close()

			for req := range in {
				fmt.Println("Request")
				fmt.Printf("req.Type: %v\n", req.Type)
				fmt.Printf("req.Payload: %s\n", req.Payload)
				fmt.Println("")

				payload := cleanCommand(string(req.Payload))
				switch req.Type {
				case "env":
					args := strings.Split(strings.Replace(payload, "\x00", "", -1), "\v")
					if len(args) != 2 {
						log.Error(3, FormatLog("Invalid env arguments: %#v"), args)
						continue
					}
					args[0] = strings.TrimLeft(args[0], "\x04")

					_, _, err := ExecCmd("env", args[0]+"="+args[1])
					if err != nil {
						log.Error(3, "Error while executing env command: %v", err)
						return
					}
				case "exec":
					cmdName := strings.TrimLeft(payload, "'()")
					log.Trace(FormatLog("Cleaned payload: %v"), cmdName)

					// Arguments for the `gogs serv` command
					// args := []string{"serv", "key-" + keyID, "--config=" + setting.CustomConf}

					// Call the program used to handle git actions
					args := []string{""}
					command := "ls"
					cmd := exec.Command(command, args...)
					cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmdName)

					stdout, err := cmd.StdoutPipe()
					if err != nil {
						log.Error(3, FormatLog("Error when reading command stdout: %v"), err)
						return
					}
					stderr, err := cmd.StderrPipe()
					if err != nil {
						log.Error(3, FormatLog("Error when reading command stderr: %v"), err)
						return
					}
					input, err := cmd.StdinPipe()
					if err != nil {
						log.Error(3, FormatLog("Error when reading command stdin: %v"), err)
						return
					}

					if err = cmd.Start(); err != nil {
						log.Error(3, FormatLog("Error when starting the command: %v"), err)
						return
					}

					req.Reply(true, nil)
					go io.Copy(input, ch)
					io.Copy(ch, stdout)
					io.Copy(ch.Stderr(), stderr)

					err = cmd.Wait()
					if err != nil {
						log.Error(3, FormatLog("Error during the command call: %v"), err)
						return
					}

					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				default:
				}

				fmt.Println("")
				fmt.Println("")
			}
		}(reqs)
	}
}
