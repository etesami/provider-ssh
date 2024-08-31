package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crossplane/provider-ssh/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/pkg/errors"
)

// Config is a SSH client configuration
type Config struct {
	RemoteHostIP   string `json:"hostIP"`
	RemoteHostPort string `json:"hostPort"`
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	PrivateKey     string `json:"privateKey,omitempty"`
	KnownHosts     string `json:"knownHosts,omitempty"`
}

// NewSSHClient creates a new SSHClient with supplied credentials
func NewSSHClient(ctx context.Context, data []byte) (*ssh.Client, error) { // nolint: gocyclo
	logger := log.FromContext(ctx).WithName("[SSHClient]")
	kc := Config{}
	var err error

	if err := json.Unmarshal(data, &kc); err != nil {
		return nil, errors.Wrap(err, "Cannot parse credentials")
	}

	config := &ssh.ClientConfig{}
	config.User = kc.Username

	if kc.Username == "" {
		return nil, errors.New("Username key not found in the data")
	}

	if kc.RemoteHostIP == "" {
		return nil, errors.New("Remote host key not found in the data")
	} else if ok := isValidIPv4(kc.RemoteHostIP); !ok {
		return nil, errors.New("Remote host address is not a valid: " + kc.RemoteHostIP)
	}

	if kc.RemoteHostPort == "" {
		logger.Info("Remote host port key not found in the data, using default port 22")
		kc.RemoteHostPort = "22"
	}

	var knownHostsCallback ssh.HostKeyCallback
	if kc.KnownHosts != "" {
		if knownHostsCallback, err = knownhosts.New(kc.KnownHosts); err != nil {
			return nil, errors.Wrap(err, "Failed to create known hosts callback")
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
			logger.Error(err, "Error decoding base64 private key")
		}

		signer, err := ssh.ParsePrivateKey(privateKeyBytes)
		if err != nil {
			logger.Error(err, "Failed to parse private key")
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

	// Maximum number of attempts
	maxAttempts := 3
	// Delay between retries
	delayBetweenRetries := 2 * time.Second
	remoteHost := fmt.Sprintf("%s:%s", kc.RemoteHostIP, kc.RemoteHostPort)

	var client *ssh.Client

	for attempts := 1; attempts <= maxAttempts; attempts++ {
		client, err = ssh.Dial("tcp", remoteHost, config)
		if err == nil {
			// Successful connection
			break
		}

		logger.Info(fmt.Sprintf("Failed to dial: %s with username %s, attempt %d/%d, error: %s", remoteHost, config.User, attempts, maxAttempts, err.Error()))

		// If this is not the last attempt, wait before retrying
		if attempts < maxAttempts {
			time.Sleep(delayBetweenRetries)
		}
	}

	if err != nil {
		// Final failure after all attempts
		logger.Info("All %d attempts to connect to %s failed.\n", maxAttempts, remoteHost)
		return nil, err
	}

	return client, nil
}

func isValidIPv4(inputAddress string) bool {
	// Check if the input is a valid IPv4 address
	// Check if the input is a valid IPv4 address
	ipv4Pattern := `^(\d{1,3}\.){3}\d{1,3}$`
	ipv4Regex := regexp.MustCompile(ipv4Pattern)

	// Regular expression pattern to match URL with anything[dot]anything
	urlPattern := `^[^\.]+(\.[^\.]+)+$`
	urlRegex := regexp.MustCompile(urlPattern)

	// Check if the input string matches IPv4 pattern or URL pattern
	if ipv4Regex.MatchString(inputAddress) || urlRegex.MatchString(inputAddress) {
		return true
	}
	return false
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
		return errors.Wrap(err, "Failed to write to remote file")
	}

	return nil
}

// ReplaceVariables replaces the variables in the script with the given values
func ReplaceVariables(script string, vars []v1alpha1.Variable) string {
	// variables are in the format of {{VAR_NAME}}
	// we remove the {{ and }} and replace the VAR_NAME with the value
	for _, v := range vars {
		script = strings.ReplaceAll(script, "{{"+v.Name+"}}", v.Value)
	}
	return script
}

// RunScript function execute the given script over an ssh session
func ExecuteScript(ctx context.Context, client *ssh.Client, sc string, vars []v1alpha1.Variable, suEnabled bool) (string, string, error) {
	logger := log.FromContext(ctx).WithName("[RunScript]")

	// Need to create different session for each command
	// replace the variables in the script
	sc = ReplaceVariables(sc, vars)

	// send the script to the remote host
	remoteFile := "/tmp/" + randomFileName(8)
	if err := sendFile(client, sc, remoteFile); err != nil {
		return "", "", errors.Wrap(err, "Failed to send script to remote host")
	}

	// make the tmpFile executable
	cmdExec := "chmod +x " + remoteFile

	// Run the script on the remote host
	var cmd string
	if suEnabled {
		cmd = "sudo "
	}
	cmd = cmdExec + " && " + cmd + remoteFile

	session, err := client.NewSession()
	if err != nil {
		logger.Error(err, "Failed to create session")
		return "", "", errors.Wrap(err, "Failed to create session")
	}
	defer closeSession(session)

	// Buffers to capture stdout and stderr separately
	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := session.Run(cmd); err != nil {
		return "", stderrBuf.String(), err
	}

	// Clean up the temporary file
	err = cleanUpTempFile(client, remoteFile)
	if err != nil {
		logger.Error(err, "Failed to clean up temporary file")
	}

	logger.Info(fmt.Sprintf("Script executed, len(stdout): %d, len(stderr): %d", len(stdoutBuf.String()), len(stderrBuf.String())))
	return stdoutBuf.String(), stderrBuf.String(), nil
}

func closeSession(session *ssh.Session) {
	err := session.Close()
	if err != nil {
		_ = fmt.Errorf("failed to close session: %w", err)
	}
}

func cleanUpTempFile(client *ssh.Client, tmpFile string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer closeSession(session)

	cmd := "rm -f " + tmpFile
	return session.Run(cmd)
}

func randomFileName(length int) string {
	bytes := make([]byte, length)
	_, err := rand.Read(bytes)
	if err != nil {
		panic(err)
	}
	return "tmp." + hex.EncodeToString(bytes)
}
