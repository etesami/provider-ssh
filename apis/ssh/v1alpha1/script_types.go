/*
Copyright 2022 The Crossplane Authors.

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
)

// ScriptParameters are the configurable fields of a Script.
type ScriptParameters struct {
	ConfigurableField string `json:"configurableField"`
}

// ScriptObservation are the observable fields of a Script.
type ScriptObservation struct {
	ObservableField string `json:"observableField,omitempty"`
}

// A ScriptSpec defines the desired state of a Script.
type ScriptSpec struct {
	xpv1.ResourceSpec `json:",inline"`
	ForProvider       ScriptParameters `json:"forProvider"`
}

// A ScriptStatus represents the observed state of a Script.
type ScriptStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          ScriptObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Script is an example API type.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,ssh}
type Script struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScriptSpec   `json:"spec"`
	Status ScriptStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ScriptList contains a list of Script
type ScriptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Script `json:"items"`
}

// Script type metadata.
var (
	ScriptKind             = reflect.TypeOf(Script{}).Name()
	ScriptGroupKind        = schema.GroupKind{Group: Group, Kind: ScriptKind}.String()
	ScriptKindAPIVersion   = ScriptKind + "." + SchemeGroupVersion.String()
	ScriptGroupVersionKind = SchemeGroupVersion.WithKind(ScriptKind)
)

func init() {
	SchemeBuilder.Register(&Script{}, &ScriptList{})
}
