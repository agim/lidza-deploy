package control

// Enrollment over SSH: the panel logs in to the new server once, with
// credentials the operator types for this one run, and executes the same
// install command a person would paste. The credentials are held in memory
// for the duration of the run only; they are never stored or logged.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type enrollmentSSH struct {
	// Address is where to log in when the hostname does not resolve to the
	// server yet; the hostname is used when empty.
	Address    string `json:"address"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase"`
	// HostKey, when given, is the SHA256 fingerprint the server must present.
	HostKey string `json:"host_key"`
}

func (s *enrollmentSSH) validate() error {
	s.Address, s.User, s.HostKey = strings.TrimSpace(s.Address), strings.TrimSpace(s.User), strings.TrimSpace(s.HostKey)
	if s.User == "" {
		s.User = "root"
	}
	if s.Port == 0 {
		s.Port = 22
	}
	switch {
	case !sshUserPattern(s.User):
		return errors.New("SSH user name is not valid")
	case s.Port < 1 || s.Port > 65535:
		return errors.New("SSH port is not valid")
	case s.Address != "" && net.ParseIP(s.Address) == nil && (len(s.Address) > 253 || !enrollmentHostname.MatchString(strings.ToLower(s.Address))):
		return errors.New("SSH address must be an IP address or a DNS name")
	case s.Password == "" && strings.TrimSpace(s.PrivateKey) == "":
		return errors.New("enter the SSH password or a private key")
	case len(s.Password) > 1024 || len(s.PrivateKey) > 32<<10 || len(s.Passphrase) > 1024:
		return errors.New("SSH credentials are too long")
	case s.HostKey != "" && !strings.HasPrefix(s.HostKey, "SHA256:"):
		return errors.New("host key fingerprint must look like SHA256:…")
	}
	return nil
}

func sshUserPattern(user string) bool {
	if len(user) > 32 {
		return false
	}
	for i, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return user != ""
}

// sshEnrollmentCommand downloads the install script to a private file and
// runs it as root. A non-root login goes through sudo, which reads the
// password from standard input when one was given.
func sshEnrollmentCommand(scriptURL, user string, hasPassword bool) string {
	run := "sh \"$f\""
	if user != "root" {
		if hasPassword {
			run = "sudo -S -p '' sh \"$f\""
		} else {
			run = "sudo -n sh \"$f\""
		}
	}
	return "umask 077; f=$(mktemp) || exit 1; { curl -fsSL '" + scriptURL + "' -o \"$f\" 2>/dev/null || wget -qO \"$f\" '" + scriptURL + "'; } && " + run + "; s=$?; rm -f \"$f\"; exit $s"
}

// enrollmentWriter feeds the remote output into the enrollment's log.
type enrollmentWriter struct {
	c         *Control
	id, token string
}

func (w enrollmentWriter) Write(p []byte) (int, error) {
	w.c.appendEnrollmentLog(w.id, w.token, p)
	return len(p), nil
}

// sshDial is replaced in tests.
var sshDial = func(ctx context.Context, address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

func (c *Control) runSSHEnrollment(ctx context.Context, e *Enrollment, login enrollmentSSH) {
	id, token, hostname := e.ID, e.token, e.Hostname
	fail := func(message string) {
		c.setEnrollment(id, func(e *Enrollment) {
			if e.State != "connected" {
				e.State, e.Message = "failed", message
			}
		})
	}
	defer func() {
		c.setEnrollment(id, func(e *Enrollment) {
			if e.cancel != nil {
				e.cancel()
				e.cancel = nil
			}
		})
	}()
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(login.PrivateKey); key != "" {
		var signer ssh.Signer
		var err error
		if login.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key+"\n"), []byte(login.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key + "\n"))
		}
		if err != nil {
			fail("The private key could not be read; paste the whole key, and its passphrase if it has one")
			return
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if login.Password != "" {
		methods = append(methods, ssh.Password(login.Password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = login.Password
			}
			return answers, nil
		}))
	}
	mismatch := false
	config := &ssh.ClientConfig{User: login.User, Auth: methods, Timeout: 20 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen := ssh.FingerprintSHA256(key)
			c.setEnrollment(id, func(e *Enrollment) { e.HostKey = seen })
			if login.HostKey != "" && login.HostKey != seen {
				mismatch = true
				return errors.New("host key mismatch")
			}
			return nil
		}}
	address := login.Address
	if address == "" {
		address = hostname
	}
	client, err := sshDial(ctx, net.JoinHostPort(address, strconv.Itoa(login.Port)), config)
	if err != nil {
		switch {
		case mismatch:
			fail("The server presented a different host key than the fingerprint you entered; nothing was run")
		case strings.Contains(err.Error(), "unable to authenticate"):
			fail("SSH login was refused for " + login.User + "; check the user name and the password or key")
		default:
			fail(fmt.Sprintf("Could not open an SSH connection to %s port %d; check the address and that SSH is reachable from the control panel", address, login.Port))
		}
		return
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		fail("The SSH session could not be opened")
		return
	}
	defer session.Close()
	out := enrollmentWriter{c: c, id: id, token: token}
	session.Stdout, session.Stderr = out, out
	stdin, err := session.StdinPipe()
	if err != nil {
		fail("The SSH session could not be opened")
		return
	}
	c.setEnrollment(id, func(e *Enrollment) { e.Message = "Logged in; installing the agent" })
	command := sshEnrollmentCommand(c.publicOrigin()+"/enroll/"+token+"/install.sh", login.User, login.Password != "")
	if err := session.Start(command); err != nil {
		fail("The install command could not be started on the server")
		return
	}
	if login.User != "root" && login.Password != "" {
		_, _ = io.WriteString(stdin, login.Password+"\n")
	}
	_ = stdin.Close()
	if err := session.Wait(); err != nil {
		if ctx.Err() != nil {
			fail("The installation was cancelled or took too long")
			return
		}
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			fail("The installer stopped with an error; its output below says why")
			return
		}
		fail("The SSH connection ended before the installer finished")
		return
	}
	c.setEnrollment(id, func(e *Enrollment) {
		if e.State != "connected" {
			e.State, e.Message = "failed", "The installer finished, but the server did not connect; its output below says why"
		}
	})
}
