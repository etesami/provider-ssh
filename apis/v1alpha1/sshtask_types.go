/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"reflect"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	corev1 "k8s.io/api/core/v1"
)

// CaptureMode controls which execution artifacts are captured.
type CaptureMode string

const (
	CaptureStdout CaptureMode = "stdout"
	CaptureStderr CaptureMode = "stderr"
	CaptureBoth   CaptureMode = "both"
	CaptureNone   CaptureMode = "none"
)

// type ConditionType xpv1.ConditionType

const (
	TypeConnected xpv1.ConditionType = "Connected"
)

type RefreshPolicy string

const (
	// RefreshPolicyAlways means run probeScript on every reconcile.
	RefreshPolicyAlways RefreshPolicy = "Always"
	// RefreshPolicyIfStale means run probeScript only when the last
	// observation is older than freshnessTTL or missing.
	RefreshPolicyIfStale RefreshPolicy = "IfStale"
)

// Script represents a shell script to be executed on the remote device.
type Script1 struct {
	// Inline shell script content.
	// +kubebuilder:validation:MinLength=1
	Inline string `json:"inline"`
}

// SSHTaskScripts groups the lifecycle scripts for convergence via SSH.
type SSHTaskScripts struct {
	// +kubebuilder:validation:Required
	ProbeScript *Script1 `json:"probeScript,omitempty"`

	// +kubebuilder:validation:Required
	EnsureScript *Script1 `json:"ensureScript,omitempty"`

	// cleanupScript runs on Delete; optional.
	CleanupScript *Script1 `json:"cleanupScript,omitempty"`
}

// ExecutionSpec controls how scripts are executed.
type ExecutionSpec struct {
	// Run commands with sudo.
	// +kubebuilder:default:=false
	Sudo *bool `json:"sudo,omitempty"`

	// Shell to use when running inline scripts.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default:="/bin/bash -euo pipefail"
	Shell *string `json:"shell,omitempty"`

	// Timeout per attempt, in seconds.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=300
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// Max attempts per reconcile (e.g., for transient failures).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=1
	MaxAttempts *int32 `json:"maxAttempts,omitempty"`

	// Environment variables to set for script execution.
	Env map[string]string `json:"env,omitempty"`
}

type FieldMapping struct {
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`

	// +kubebuilder:validation:MinLength=1
	To string `json:"to"`

  // Defaults to "jsonpath" if empty.
	// +optional
	// +kubebuilder:validation:Enum=jsonpath;jq
	Engine *string `json:"engine,omitempty"`
}

// ObservePolicySpec controls how probeScript runs and its results are stored.
type ObservePolicySpec struct {
	// When to run probeScript. Default: IfStale.
	// +optional
	// +kubebuilder:validation:Enum=Always;IfStale
	RefreshPolicy *RefreshPolicy `json:"refreshPolicy,omitempty"`

	// How long an observation stays fresh (IfStale). Default: 60s.
	// +optional
	FreshnessTTL *metav1.Duration `json:"freshnessTTL,omitempty"`

	// Which output streams from probeScript to keep. Default: stdout.
	// +optional
	// +kubebuilder:validation:Enum=stdout;stderr;both;none
	Capture *CaptureMode `json:"capture,omitempty"`

	// Map contains rules to extract specific fields from the probe "facts"
	// +optional
	Map []FieldMapping `json:"map,omitempty"`
}

// ArtifactPolicySpec controls how execution artifacts are captured.
type ArtifactPolicySpec struct {
	// Which stream(s) to capture from the remote execution.
	// +kubebuilder:validation:Enum=stdout;stderr;both;none
	// +kubebuilder:default:=none
	Capture CaptureMode `json:"capture,omitempty"`
}

// SSHTaskParameters are the configurable fields of a SSHTask.
type SSHTaskParameters struct {
	// scripts to evaluate/apply/diff/cleanup the target state.
	// +kubebuilder:validation:Required
	Scripts SSHTaskScripts `json:"scripts"`

	// execution environment and safety controls.
	Execution *ExecutionSpec `json:"execution,omitempty"`

	// artifact capture policy.
	ArtifactPolicy *ArtifactPolicySpec `json:"artifactPolicy,omitempty"`

	// observation policy.
	Observe *ObservePolicySpec `json:"observe,omitempty"`
}

// ObservedStatus holds the last successful probe results.
type ObservedStatus struct {
	// Raw JSON facts from probeScript.
	// +optional
	Raw *apiextensionsv1.JSON `json:"raw,omitempty"`

	// Fields extracted from Raw via mapping rules.
	// +optional
	Fields map[string]string `json:"fields,omitempty"`

	// Digest hash of Raw (e.g., sha256:<hex>).
	// +optional
	Digest *string `json:"digest,omitempty"`

	// Time when probe completed.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Drift info reported by probe.
	// +optional
	Drift *apiextensionsv1.JSON `json:"drift,omitempty"`
}

// Endpoint describes the target SSH endpoint that was used/observed.
type Endpoint struct {
	Host     string `json:"host,omitempty"`
	Port     int32  `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
}

// LastRunStatus describes the most recent execution attempt.
type LastRunStatus struct {
	Time       *metav1.Time `json:"time,omitempty"`
	ExitCode   *int32       `json:"exitCode,omitempty"`
	RetryCount *int32       `json:"retryCount,omitempty"`
}

// Artifacts holds captured stdout/stderr from the last execution.
type Artifacts struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

// SSHTaskObservation are the observable fields of a SSHTask.
type SSHTaskObservation struct {
	Endpoint      *Endpoint     `json:"endpoint,omitempty"`
	LastCheckTime *metav1.Time  `json:"lastCheckTime,omitempty"`
	LastRun       *LastRunStatus`json:"lastRun,omitempty"`
	Artifacts     *Artifacts    `json:"artifacts,omitempty"`
	Observed      *ObservedStatus `json:"observed,omitempty"`
}

// A SSHTaskSpec defines the desired state of a SSHTask.
type SSHTaskSpec struct {
	xpv1.ResourceSpec `json:",inline"`
	ForProvider SSHTaskParameters `json:"forProvider"`
}

// A SSHTaskStatus represents the observed state of a SSHTask.
type SSHTaskStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          SSHTaskObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A SSHTask is an example API type.
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="CONNECTED",type="string",JSONPath=".status.conditions[?(@.type=='Connected')].status"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,ssh}
type SSHTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SSHTaskSpec   `json:"spec"`
	Status SSHTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SSHTaskList contains a list of SSHTask
type SSHTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHTask `json:"items"`
}

// SSHTask type metadata.
var (
	SSHTaskKind             = reflect.TypeOf(SSHTask{}).Name()
	SSHTaskGroupKind        = schema.GroupKind{Group: Group, Kind: SSHTaskKind}.String()
	SSHTaskKindAPIVersion   = SSHTaskKind + "." + SchemeGroupVersion.String()
	SSHTaskGroupVersionKind = SchemeGroupVersion.WithKind(SSHTaskKind)
)

func init() {
	SchemeBuilder.Register(&SSHTask{}, &SSHTaskList{})
}


func (in *SSHTask) SetCondition(conditionType xpv1.ConditionType, status corev1.ConditionStatus, reason xpv1.ConditionReason, message string) {
	in.Status.SetConditions(xpv1.Condition{
		Type:    conditionType,
		Status:  status,
		LastTransitionTime: metav1.Now(),
		Reason:  reason,
		Message: message,
	})
}