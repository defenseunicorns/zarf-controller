/*
Copyright 2022.

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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ZarfFinalizer = "finalizers.zarf.dev"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InstallationSpec defines the desired state of Installation
type InstallationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Approve auto approve the installation of new zarf things
	// +optional
	Approve string `json:"Approve,omitempty"`

	// List of input variables to set for the Terraform program.
	// +optional
	Vars []Variable `json:"vars,omitempty"`

	// SourceRef is the reference of the source where the Terraform files are stored.
	// +required
	SourceRef CrossNamespaceSourceReference `json:"sourceRef"`

	// Path is the locaion in the repo
	// +optional
	Path string `json:"path,omitempty"`

	// Suspend is to tell the controller to suspend subsequent TF executions,
	// it does not apply to already started executions. Defaults to false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Force instructs the controller to unconditionally
	// re-plan and re-apply TF resources. Defaults to false.
	// +kubebuilder:default:=false
	// +optional
	Force bool `json:"force,omitempty"`

	// The interval at which to reconcile the Terraform.
	// +required
	Interval metav1.Duration `json:"interval"`
}

// CrossNamespaceSourceReference contains enough information to let you locate the
// typed Kubernetes resource object at cluster level.
type CrossNamespaceSourceReference struct {
	// API version of the referent.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referent.
	// +kubebuilder:validation:Enum=GitRepository;Bucket;OCIRepository
	// +required
	Kind string `json:"kind"`

	// Name of the referent.
	// +required
	Name string `json:"name"`

	// Namespace of the referent, defaults to the namespace of the Kubernetes resource object that contains the reference.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

type Variable struct {
	// Name is the name of the variable
	// +required
	Name string `json:"name"`

	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// +optional
	ValueFrom *corev1.EnvVarSource `json:"valueFrom,omitempty"`
}

// InstallationStatus defines the observed state of Installation
type InstallationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PackageName name for the installation
	PackageName string `json:"packageName,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Installation is the Schema for the installations API
type Installation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InstallationSpec   `json:"spec,omitempty"`
	Status InstallationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// InstallationList contains a list of Installation
type InstallationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Installation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Installation{}, &InstallationList{})
}
