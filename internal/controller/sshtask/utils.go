package sshtask

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1a1 "github.com/etesami/provider-ssh/apis/v1alpha1"
	ssh "golang.org/x/crypto/ssh"
)


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


func getConnectionConfigFromSecret(data []byte) (*config, error) {
	kc := config{}
	if err := json.Unmarshal(data, &kc); err != nil {
		return nil, errors.Wrap(err, "Cannot parse credentials")
	}

	if kc.Username == "" {
		return nil, errors.New("Username key not found in the data")
	}

	if kc.PrivateKey == "" && kc.Password == "" {
		return nil, errors.New("Private Key or Password key not found in the data.")
	}

	if kc.RemoteHostIP == "" {
		return nil, errors.New("Remote host key not found in the data")
	} else if ok := isValidIPv4(kc.RemoteHostIP); !ok {
		return nil, errors.New("Remote host address is not a valid: " + kc.RemoteHostIP)
	}

	if kc.RemoteHostPort == "" {
		// Default port 22
		kc.RemoteHostPort = "22"
	}
	return &kc, nil
}


func testConnection(client *ssh.Client, timeoutSeconds int) bool {
	counter := 0
	max_attempts := 5
	for counter < max_attempts {

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds) * time.Second)
		defer cancel()

		_, _, err := runScriptWithTimeout(ctx, client, "echo 'test'", nil, false)
		if err == nil { 
			return true 
		}

		counter++
	}
	return false
}

func computeDigest(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:])
}

func isObserveStale(cr *apiv1a1.SSHTask) bool {
	// Default behavior: IfStale with 60s TTL if not specified.
	p := cr.Spec.ForProvider.Observe
	ttl := 60 * time.Second
	if p.FreshnessTTL != nil {
		ttl = p.FreshnessTTL.Duration
	}
	refresh := apiv1a1.RefreshPolicyIfStale
	if p.RefreshPolicy != nil {
		refresh = *p.RefreshPolicy
	}
	if refresh == apiv1a1.RefreshPolicyAlways {
		return true
	}
	obs := cr.Status.AtProvider.Observed
	if obs == nil || obs.ObservedAt == nil {
		return true
	}
	return time.Since(obs.ObservedAt.Time) > ttl
}

// mapObservedFields maps values from the "facts" payload to status.atProvider.observed.fields
// For simplicity, this implementation stores the
// entire JSON facts under Observed.Fields["raw"]
func mapObservedFields(cr *apiv1a1.SSHTask, facts json.RawMessage) {
	// obsCfg := cr.Spec.ForProvider.Observe
	if cr.Status.AtProvider.Observed == nil {
		cr.Status.AtProvider.Observed = &apiv1a1.ObservedStatus{}
	}

	// Reset the Fields to remove all old key and values
	cr.Status.AtProvider.Observed.Fields = map[string]string{}
	// if cr.Status.AtProvider.Observed.Fields == nil {
	// 	cr.Status.AtProvider.Observed.Fields = map[string]string{}
	// }

	// If no mapping rules, persist compact raw JSON as a convenience.
	if len(cr.Spec.ForProvider.Observe.Map) == 0 {
		cr.Status.AtProvider.Observed.Fields["raw"] = compactJSON(facts)
		return
	}

	// Decode once for all mappings.
	var doc any
	if err := json.Unmarshal(facts, &doc); err != nil {
		// On parse error, stash raw for troubleshooting and return.
		cr.Status.AtProvider.Observed.Fields["raw"] = string(facts)
		cr.Status.AtProvider.Observed.Fields["_mapError"] = "invalid JSON facts"
		return
	}

	root := map[string]any{"raw": doc}

	for _, m := range cr.Spec.ForProvider.Observe.Map {
		if strings.TrimSpace(m.To) == "" || strings.TrimSpace(m.From) == "" {
			continue
		}

		// Build path: raw.<From...>
		path := append([]string{"raw"}, strings.Split(m.From, ".")...)

		v, err := getNestedValue(root, path...)
		if err != nil {
			key := fmt.Sprintf("_mapError_%s", m.From)
			cr.Status.AtProvider.Observed.Fields[key] = err.Error()
			continue
		}
		
		// Normalize value to string for Observed.Fields:
		// - strings stay as-is
		// - scalars -> fmt.Sprint
		// - objects/arrays -> compact JSON
		switch val := v.(type) {
		case string:
			cr.Status.AtProvider.Observed.Fields[m.To] = val
		case json.Number: // if getNestedValue preserves numbers as json.Number
			cr.Status.AtProvider.Observed.Fields[m.To] = val.String()
		case nil, bool, float32, float64, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64:
			cr.Status.AtProvider.Observed.Fields[m.To] = fmt.Sprint(val)
		default:
			// Marshal complex types to compact JSON for readability.
			b, marshalErr := json.Marshal(val)
			if marshalErr != nil {
				key := fmt.Sprintf("_mapError_%s", m.From)
				cr.Status.AtProvider.Observed.Fields[key] = fmt.Sprintf("marshal error: %v", marshalErr)
				continue
			}
			cr.Status.AtProvider.Observed.Fields[m.To] = string(b)
		}
	}

	// Always keep a compact copy of the raw JSON for reference.
	// cr.Status.AtProvider.Observed.Fields["raw"] = compactJSON(facts)
}

func compactJSON(b json.RawMessage) string {
	if len(b) == 0 {
		return ""
	}
	var tmp any
	if err := json.Unmarshal(b, &tmp); err != nil {
		return string(b)
	}
	out, err := json.Marshal(tmp)
	if err != nil {
		return string(b)
	}
	return string(out)
}

func strPtr(s string) *string { return &s }


// ReplaceVariables replaces the variables in the script with the given values
func replaceVariables(script string, vars []apiv1a1.EnvSpec) string {
	// variables are in the format of {{VAR_NAME}}
	// we remove the {{ and }} and replace the VAR_NAME with the value
	for _, v := range vars {
		script = strings.ReplaceAll(script, v.Name, v.Value)
	}
	return script
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


func selectArtifacts(mode apiv1a1.CaptureMode, stdout, stderr string) *apiv1a1.Artifacts {
	switch mode {
	case apiv1a1.CaptureStdout:
		return &apiv1a1.Artifacts{Stdout: stdout}
	case apiv1a1.CaptureStderr:
		return &apiv1a1.Artifacts{Stderr: stderr}
	case apiv1a1.CaptureBoth:
		return &apiv1a1.Artifacts{Stdout: stdout, Stderr: stderr}
	default:
		return &apiv1a1.Artifacts{}
	}
}

func defaultExecution(in *apiv1a1.ExecutionSpec) *apiv1a1.ExecutionSpec {
	// Defaults are set by CRD, but ensure safe defaults here too.
	defShell := "/bin/bash -euo pipefail"
	defTimeout := int32(300)
	defAttempts := int32(1)
	defSudo := false

	if in == nil {
		return &apiv1a1.ExecutionSpec{
			Sudo:           &defSudo,
			Shell:          &defShell,
			TimeoutSeconds: &defTimeout,
			MaxAttempts:    &defAttempts,
			Env:            []apiv1a1.EnvSpec{},
		}
	}
	out := *in
	if out.Sudo == nil {
		out.Sudo = &defSudo
	}
	if out.Shell == nil || strings.TrimSpace(*out.Shell) == "" {
		out.Shell = &defShell
	}
	if out.TimeoutSeconds == nil || *out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = &defTimeout
	}
	if out.MaxAttempts == nil || *out.MaxAttempts <= 0 {
		out.MaxAttempts = &defAttempts
	}
	if out.Env == nil {
		out.Env = []apiv1a1.EnvSpec{}
	}
	return &out
}

func defaultCapture(in *apiv1a1.ArtifactPolicySpec) apiv1a1.CaptureMode {
	if in == nil {
		return apiv1a1.CaptureNone
	}
	if in.Capture == "" {
		return apiv1a1.CaptureNone
	}
	return in.Capture
}


func shellQuote(s string) string {
	// single-quote and escape existing single quotes: ' -> '"'"'
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func populateEndpoint(cr *apiv1a1.SSHTask, cfg *config) {
	if cfg == nil {
		return
	}
	cr.Status.AtProvider.Endpoint = &apiv1a1.Endpoint{
		Host:     cfg.RemoteHostIP,
		Username: cfg.Username,
	}
	// parse port
	var p int32 = 22
	if cfg.RemoteHostPort != "" {
		if v, err := parseInt32(cfg.RemoteHostPort); err == nil {
			p = v
		}
	}
	cr.Status.AtProvider.Endpoint.Port = p
}

func metav1Now() metav1.Time {
	return metav1.NewTime(time.Now().UTC())
}

func int32Ptr(v int32) *int32 { return &v }

// parseInt32 best-effort.
func parseInt32(s string) (int32, error) {
	i, err := strconv.ParseInt(s, 10, 32)
	return int32(i), err
}