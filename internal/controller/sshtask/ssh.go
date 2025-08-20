package sshtask

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	apiv1a1 "github.com/etesami/provider-ssh/apis/v1alpha1"
)

// Config is a SSH client configuration
type config struct {
	RemoteHostIP   string `json:"hostIP"`
	RemoteHostPort string `json:"hostPort"`
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	PrivateKey     string `json:"privateKey,omitempty"`
	KnownHosts     string `json:"knownHosts,omitempty"`
}


func newSSHClient(kc *config) (*ssh.Client, error) {
	config := &ssh.ClientConfig{}

	config.User = kc.Username

	var knownHostsCallback ssh.HostKeyCallback

	if kc.KnownHosts != "" {
		tempFile, err := os.CreateTemp("", "tempfile")
		if err != nil {
			return nil, errors.Wrap(err, "failed to create temp file for known hosts")
		}
		defer os.Remove(tempFile.Name()) // Clean up the temp file after use

		// Write the content to the temporary file
		if _, err := tempFile.Write([]byte(kc.KnownHosts)); err != nil {
			return nil, errors.Wrap(err, "failed to write known hosts to temp file")
		}
		defer tempFile.Close()
		if knownHostsCallback, err = knownhosts.New(tempFile.Name()); err != nil {
			return nil, errors.Wrap(err, "failed to create known hosts callback")
		}
	} else {
		// If knownHosts is not provided, use InsecureIgnoreHostKey
		// This is not recommended for production use
		// nolint: gosec
		knownHostsCallback = ssh.InsecureIgnoreHostKey()
	}
	config.HostKeyCallback = knownHostsCallback

	switch {
	case kc.PrivateKey != "":
		privateKeyBytes, err := base64.StdEncoding.DecodeString(kc.PrivateKey)
		if err != nil {
			return nil, errors.Wrap(err, "error decoding base64 private key")
		}
		signer, err := ssh.ParsePrivateKey(privateKeyBytes)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse private key")
		}
		config.Auth = []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		}

	case kc.Password != "":
		config.Auth = []ssh.AuthMethod{
			ssh.Password(kc.Password), // Replace with your remote server password
		}
	default:
		return nil, errors.New("Private Key or Password key not found in the data.")
	}

	// Maximum number of attempts, Delay between retries
	maxAttempts, delayBetweenRetries := 3, 3 * time.Second
	
	config.Timeout = 10 * time.Second
	remoteHost := fmt.Sprintf("%s:%s", kc.RemoteHostIP, kc.RemoteHostPort)

	var client *ssh.Client
	var err error

	for attempts := 1; attempts <= maxAttempts; attempts++ {
		client, err = ssh.Dial("tcp", remoteHost, config)
		if err == nil { return client, nil } // Connection successful
		
		// If this is not the last attempt, wait before retrying
		if attempts < maxAttempts { time.Sleep(delayBetweenRetries)}
	}

	msg := fmt.Sprintf("Failed to connect to %s after %d attempts", remoteHost, maxAttempts)
	return nil, errors.Wrap(err, msg)
}

func runScriptWithTimeout(ctx context.Context, client *ssh.Client, sc string, vars map[string]string, sudo bool) (string, string, error) {
	
	type result struct {
		stdout string
		stderr string
		err    error
	}
	resultChan := make(chan result, 1)
	go func() {
		stdout, stderr, err := runScript(client, sc, vars, sudo)
		resultChan <- result{stdout: stdout, stderr: stderr, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", "", errors.New("timeout")
	case res := <-resultChan:
		return res.stdout, res.stderr, res.err
	}
}

func closeSession(session *ssh.Session) {
	err := session.Close()
	if err != nil {
		_ = fmt.Errorf("failed to close session: %w", err)
	}
}

// send a file to the remote host
func sendFile(client *ssh.Client, fileContent, remotePath string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer closeSession(session)

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer func() {
		err := session.Close()
		if err != nil {
			_ = fmt.Errorf("failed to close sftp session: %w", err)
		}
	}()

	// Convert the string content to a byte buffer
	fileBuffer := bytes.NewBufferString(fileContent)

	// Open the destination file on the remote host
	remoteFile, err := sftpClient.Create(remotePath)
	if err != nil {
		return errors.Wrap(err, "Failed to create remote file")
	}
	defer func() {
		err = remoteFile.Close()
		if err != nil {
			_ = fmt.Errorf("failed to close remote file: %w", err)
		}
	}()

	// Write the file content to the remote file
	_, err = fileBuffer.WriteTo(remoteFile)
	if err != nil {
		return errors.Wrap(err, "failed to write to remote file")
	}

	return nil
}


// RunScript function execute the given script over an ssh session
func runScript(client *ssh.Client, sc string, vars map[string]string, sudo bool) (string, string, error) {

	// Need to create different session for each command
	// replace the variables in the script
	sc = replaceVariables(sc, vars)

	// send the script to the remote host
	remoteFile := "/tmp/" + randomFileName(8)
	if err := sendFile(client, sc, remoteFile); err != nil {
		return "", "", errors.Wrap(err, "failed to send script to remote host")
	}

	// make the tmpFile executable
	cmdExec := "chmod +x " + remoteFile

	// Run the script on the remote host
	var cmd string
	if sudo {
		cmd = "sudo "
	}
	cmd = cmdExec + " && " + cmd + remoteFile

	session, err := client.NewSession()
	if err != nil {
		return "", "", errors.Wrap(err, "failed to create session")
	}
	defer closeSession(session)

	// Buffers to capture stdout and stderr separately
	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := session.Run(cmd); err != nil {
		return "", stderrBuf.String(), err
	}

	// Clean up the temporary file (best effort)
	_ = cleanUpTempFile(client, remoteFile)
	return stdoutBuf.String(), stderrBuf.String(), nil
}

// RunScript function execute the given script over an ssh session
func runScript2(ctx context.Context, cli *ssh.Client, sc string, exec *apiv1a1.ExecutionSpec, cap apiv1a1.CaptureMode) (exitCode int, stdout, stderr string, dur time.Duration, err error) {

	start := time.Now()
	defer func() { dur = time.Since(start) }()

	if strings.TrimSpace(sc) == "" {
		return 0, "", "", 0, nil
	}

	session, err := cli.NewSession()
	if err != nil {
		return -1, "", "", 0, errors.Wrap(err, "failed to create session")
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	if cap == apiv1a1.CaptureStdout || cap == apiv1a1.CaptureBoth {
		session.Stdout = &outBuf
	}
		if cap == apiv1a1.CaptureStderr || cap == apiv1a1.CaptureBoth {
		session.Stderr = &errBuf
	}

	// Build command
	shell := strings.TrimSpace(*exec.Shell)

	sc = replaceVariables(sc, exec.Env)
	quoted := shellQuote(sc)
	cmd := fmt.Sprintf("%s -c %s", shell, quoted)
	if exec.Sudo != nil && *exec.Sudo {
		cmd = fmt.Sprintf("sudo -n %s", cmd)
	}


	// Apply timeout using a derived context.
	tout := time.Duration(*exec.TimeoutSeconds) * time.Second
	ctx2, cancel := context.WithTimeout(ctx, tout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case <-ctx2.Done():
		_ = session.Signal(ssh.SIGKILL) // best-effort; may not be supported on all servers
		_ = session.Close()
		return -1, outBuf.String(), errBuf.String(), time.Since(start), ctx2.Err()
	case runErr := <-done:
		if runErr == nil {
			return 0, outBuf.String(), errBuf.String(), time.Since(start), nil
		}
		exit := -1
		if ee, ok := runErr.(*ssh.ExitError); ok {
			exit = ee.ExitStatus()
		}
		return exit, outBuf.String(), errBuf.String(), time.Since(start), runErr
	}
}