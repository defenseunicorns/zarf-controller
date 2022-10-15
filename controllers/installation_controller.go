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

package controllers

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/go-retryablehttp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/k8s"
	"github.com/defenseunicorns/zarf/src/packager"
	ztypes "github.com/defenseunicorns/zarf/src/types"
	"github.com/fluxcd/pkg/http/fetch"
	"github.com/fluxcd/pkg/tar"

	zarfdevv1beta1 "github.com/defenseunicorns/zarf-controller/api/v1beta1"
)

// InstallationReconciler reconciles a Installation object
type InstallationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	httpClient    *retryablehttp.Client
	statusManager string

	artifactFetcher *fetch.ArchiveFetcher
}

// +kubebuilder:rbac:groups=zarf.dev,resources=installations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zarf.dev,resources=installations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zarf.dev,resources=installations/finalizers,verbs=update
// +kubebuilder:rbac:groups=zarf.dev,resources=installations/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=*,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=*,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="*",resources="*",verbs="*"
// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *InstallationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info(fmt.Sprintf("Got request for object %v", req.NamespacedName))
	var install zarfdevv1beta1.Installation
	if err := r.Get(ctx, req.NamespacedName, &install); err != nil {
		l.Error(err, "unable to fetch Installation")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Add our finalizer if it does not exist
	if !controllerutil.ContainsFinalizer(&install, zarfdevv1beta1.ZarfFinalizer) {
		patch := client.MergeFrom(install.DeepCopy())
		controllerutil.AddFinalizer(&install, zarfdevv1beta1.ZarfFinalizer)
		if err := r.Patch(ctx, &install, patch, client.FieldOwner(r.statusManager)); err != nil {
			l.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}
	l.Info("Object has finalizer now")

	// Examine if the object is under deletion
	if !install.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info(fmt.Sprintf("Looking to delete object %v", req.NamespacedName))
		return r.finalize(ctx, install)
	}
	l.Info("Object is not flagged for deletion")

	if install.Spec.Suspend {
		l.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}
	l.Info("Object is not suspended")

	sourceObj, err := r.getSource(ctx, install)
	if err != nil {
		l.Error(err, "error getting source for installation")
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Source '%s' not found.  Requeueing after a minute", install.Spec.SourceRef.Name)
			l.Error(err, msg)
			// install.Status.Conditions = []metav1.Condition{metav1.Condition{
			// 	Type:    meta.ReadyCondition,
			// 	Status:  metav1.ConditionFalse,
			// 	Reason:  msg,
			// 	Message: msg,
			// },
			// }

			// if err := r.patchStatus(ctx, req.NamespacedName, install.Status); err != nil {
			// 	l.Error(err, "unable to update status for source not found")
			// 	return ctrl.Result{Requeue: true}, err
			// }
			// do not requeue immediately, when the source is created the watcher should trigger a reconciliation
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else {
			// retry on transient errors
			l.Error(err, "retry")
			return ctrl.Result{Requeue: true}, err
		}
	}
	b, _ := json.MarshalIndent(sourceObj, "", "\t")

	l.Info(fmt.Sprintf("Found source object: \n %v", string(b)))
	// sourceObj does not exist, return early
	if sourceObj.GetArtifact() == nil {
		msg := "Source is not ready, artifact not found"
		install.Status.Conditions = []metav1.Condition{metav1.Condition{
			Type:    meta.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  msg,
			Message: msg,
		},
		}

		if err := r.patchStatus(ctx, req.NamespacedName, install.Status); err != nil {
			l.Error(err, "unable to update status for source not found")
			return ctrl.Result{Requeue: true}, err
		}
		l.Info(msg)
		// do not requeue immediately, when the artifact is created the watcher should trigger a reconciliation
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	art := sourceObj.GetArtifact()
	l.Info(fmt.Sprintf("Downloaded artifact to %v", art.Path))
	b, _ = json.MarshalIndent(art, "", "\t")
	l.Info(fmt.Sprintf("\nArtifact:\n%v\n", string(b)))

	dir, err := r.download(ctx, art)

	if err != nil {
		l.Error(err, fmt.Sprintf("Error downloading %v", art.URL))
		return ctrl.Result{Requeue: true}, err
	}
	// defer os.RemoveAll(dir)
	l.Info(fmt.Sprintf("Made tempdir at %v", dir))

	baseDir := filepath.Join(dir, install.Spec.Path)

	// if _, err := os.Stat(filepath.Join(baseDir, config.ZarfYAML)); errors.Is(err, os.ErrNotExist) {
	// 	l.Error(err, fmt.Sprintf("Could not find path %v in source %v", install.Spec.Path, sourceObj))
	// 	return ctrl.Result{RequeueAfter: time.Minute}, err
	// }

	// Not sure if I still need to do this
	state, err := k8s.LoadZarfState()
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	config.InitState(state)
	//auto confirm
	config.CommonOptions.Confirm = true

	config.CliArch = "amd64"
	//see if this folder has a Zarf.yaml file in  it, otherwise its an oci already built

	if _, err := os.Stat(filepath.Join(baseDir, config.ZarfYAML)); errors.Is(err, os.ErrNotExist) {
		// path/to/whatever does not exist
		//already an OCI object
		l.Info(fmt.Sprintf("No Zarf.yaml found in folder %v.  Assuming prebuild object.", baseDir))
		files, err := filepath.Glob(dir + "/*.tar*")
		if err != nil {
			l.Error(err, "Error getting tar from folder")
			return ctrl.Result{Requeue: true}, err
		}
		config.DeployOptions.PackagePath = files[0]
		// Would like to get the pakage name here so I can delete it safely later
		// err = r.patchStatus(ctx, req.NamespacedName, zarfdevv1beta1.InstallationStatus{
		// 	PackageName: zPackage.Metadata.Name,
		// 	// Conditions: []metav1.Condition{{
		// 	// 	Type:    meta.ReadyCondition,
		// 	// 	Status:  metav1.ConditionTrue,
		// 	// 	Reason:  "Healthy",
		// 	// 	Message: "Healthy",
		// 	// },
		// 	// }})
		// })
		// if err != nil {
		// 	l.Error(err, fmt.Sprintf("Error patching status: %v", err))
		// }

	} else {
		// zarf.yaml in the path, so lets build one
		// Build it into the same location as the zarf.yaml
		config.CreateOptions.OutputDirectory = baseDir
		// Would like this to return an error instead of exiting
		packager.Create(baseDir)
		//wont work for more than one yet, but yolo
		files, err := filepath.Glob(baseDir + "/" + "*.tar*")
		if len(files) == 0 {
			l.Error(err, fmt.Sprintf("No tar files found in %v", baseDir))
			return ctrl.Result{Requeue: true}, err
		}
		config.DeployOptions.PackagePath = files[0]
		configPath := filepath.Join(dir, install.Spec.Path, config.ZarfYAML)
		content, err := os.ReadFile(configPath)

		// Convert []byte to string and print to screen
		zPackage := ztypes.ZarfPackage{}

		err = yaml.Unmarshal(content, &zPackage)

		// Save the packageName so we can remove it later
		err = r.patchStatus(ctx, req.NamespacedName, zarfdevv1beta1.InstallationStatus{
			PackageName: zPackage.Metadata.Name,
			// Conditions: []metav1.Condition{{
			// 	Type:    meta.ReadyCondition,
			// 	Status:  metav1.ConditionTrue,
			// 	Reason:  "Healthy",
			// 	Message: "Healthy",
			// },
			// }})
		})
		if err != nil {
			l.Error(err, fmt.Sprintf("Error patching status: %v", err))
		}
	}

	b, _ = json.MarshalIndent(state, "", "\t")
	l.Info(string(b))

	// Now deploy it!
	packager.Deploy()
	l.Info("Deployment successful")

	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstallationReconciler) SetupWithManager(mgr ctrl.Manager) error {

	r.artifactFetcher = fetch.NewArchiveFetcher(5, tar.UnlimitedUntarSize, os.Getenv("SOURCE_CONTROLLER_LOCALHOST"))

	// do zarf init things?
	httpClient := retryablehttp.NewClient()
	httpClient.RetryWaitMin = 5 * time.Second
	httpClient.RetryWaitMax = 30 * time.Second
	httpClient.RetryMax = 5
	httpClient.HTTPClient.Timeout = 60 * time.Second
	httpClient.Logger = nil
	r.httpClient = httpClient

	return ctrl.NewControllerManagedBy(mgr).
		For(&zarfdevv1beta1.Installation{}).
		Complete(r)

}

func (r *InstallationReconciler) patchStatus(ctx context.Context, objectKey types.NamespacedName, newStatus zarfdevv1beta1.InstallationStatus) error {
	var install zarfdevv1beta1.Installation
	if err := r.Get(ctx, objectKey, &install); err != nil {
		return err
	}

	patch := client.MergeFrom(install.DeepCopy())
	install.Status = newStatus

	err := r.Status().Patch(ctx, &install, patch, client.FieldOwner(r.statusManager))

	if err != nil {
		log.Log.Error(err, "Error patching installation status")
	}
	return err
}

func (r *InstallationReconciler) getSource(ctx context.Context, install zarfdevv1beta1.Installation) (sourcev1.Source, error) {
	var sourceObj sourcev1.Source
	sourceNamespace := install.GetNamespace()
	if install.Spec.SourceRef.Namespace != "" {
		sourceNamespace = install.Spec.SourceRef.Namespace
	}
	namespacedName := types.NamespacedName{
		Namespace: sourceNamespace,
		Name:      install.Spec.SourceRef.Name,
	}
	switch install.Spec.SourceRef.Kind {
	case sourcev1.GitRepositoryKind:
		var repository sourcev1.GitRepository
		err := r.Client.Get(ctx, namespacedName, &repository)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &repository
	case sourcev1.BucketKind:
		var bucket sourcev1.Bucket
		err := r.Client.Get(ctx, namespacedName, &bucket)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &bucket
	case sourcev1.OCIRepositoryKind:
		var repository sourcev1.OCIRepository
		err := r.Client.Get(ctx, namespacedName, &repository)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &repository
	default:
		return sourceObj, fmt.Errorf("source `%s` kind '%s' not supported",
			install.Spec.SourceRef.Name, install.Spec.SourceRef.Kind)
	}
	return sourceObj, nil
}

func (install InstallationReconciler) download(ctx context.Context, artifact *sourcev1.Artifact) (string, error) {
	artifactURL := artifact.URL
	l := log.FromContext(ctx)
	l.Info(fmt.Sprintf("Default Artifact URL to %v", artifactURL))
	if hostname := os.Getenv("SOURCE_CONTROLLER_LOCALHOST"); hostname != "" {
		u, err := url.Parse(artifactURL)
		if err != nil {
			return "", err
		}
		u.Host = hostname
		if strings.HasSuffix(hostname, "bigbang.dev") {
			u.Scheme = "https"
		}
		artifactURL = u.String()
		log.Log.Info(fmt.Sprintf("Updating Artifact URL to %v b/c of local environment variable", artifactURL))
	}
	// Generated by curl-to-Go: https://mholt.github.io/curl-to-go

	// curl localhost:9090/gitrepository/default/zarf/latest.tar.gz

	// create tmp dir
	tmpDir, err := MkdirTempAbs(filepath.Join(os.TempDir(), "zarf"), artifact.Checksum)
	if err != nil {

		err = fmt.Errorf("tmp dir error: %w", err)
		l.Error(err, "Error making temp dir")
		return "", err
	}

	// download artifact and extract files
	l.Info("About to call Fetch")
	err = install.artifactFetcher.Fetch(artifactURL, artifact.Checksum, tmpDir)
	return tmpDir, err

	// // check build path exists
	// dirPath, err := securejoin.SecureJoin(tmpDir, kustomization.Spec.Path)
	// if err != nil {
	// 	return err
	// }
	// if _, err := os.Stat(dirPath); err != nil {
	// 	err = fmt.Errorf("kustomization path not found: %w", err)
	// 	return kustomizev1.KustomizationNotReady(
	// 		kustomization,
	// 		revision,
	// 		kustomizev1.ArtifactFailedReason,
	// 		err.Error(),
	// 	), err
	// }

	// req, err := retryablehttp.NewRequest(http.MethodGet, artifactURL, nil)
	// if err != nil {
	// 	return fmt.Errorf("failed to create a new request: %w", err)
	// }

	// resp, err := install.httpClient.Do(req)
	// if err != nil {
	// 	return fmt.Errorf("failed to download artifact, error: %w", err)
	// }
	// defer resp.Body.Close()

	// // check response
	// if resp.StatusCode != http.StatusOK {
	// 	return fmt.Errorf("failed to download artifact from %s, status: %s", artifactURL, resp.Status)
	// }

	// log.Log.Info(fmt.Sprintf("Downloaded artifact: Status: %v,  Size: %v, ", resp.Status, resp.ContentLength))

	// var buf bytes.Buffer

	// // verify checksum matches origin
	// if err := install.verifyArtifact(artifact, &buf, resp.Body); err != nil {
	// 	return err
	// }

	// // extract
	// if _, err = untar.Untar(&buf, tmpDir); err != nil {
	// 	return fmt.Errorf("failed to untar artifact, error: %w", err)
	// }

	// return nil
}

func (r *InstallationReconciler) verifyArtifact(artifact *sourcev1.Artifact, buf *bytes.Buffer, reader io.Reader) error {
	hasher := sha256.New()

	// for backwards compatibility with source-controller v0.17.2 and older
	if len(artifact.Checksum) == 40 {
		hasher = sha1.New()
	}

	// compute checksum
	mw := io.MultiWriter(hasher, buf)
	if _, err := io.Copy(mw, reader); err != nil {
		return err
	}

	if checksum := fmt.Sprintf("%x", hasher.Sum(nil)); checksum != artifact.Checksum {
		return fmt.Errorf("failed to verify artifact: computed checksum '%s' doesn't match advertised '%s'",
			checksum, artifact.Checksum)
	}

	return nil
}

// MkdirTempAbs creates a tmp dir and returns the absolute path to the dir.
// This is required since certain OSes like MacOS create temporary files in
// e.g. `/private/var`, to which `/var` is a symlink.
func MkdirTempAbs(dir, pattern string) (string, error) {
	tmpdir := filepath.Join(dir, pattern)
	err := os.MkdirAll(tmpdir, 0755)
	return tmpdir, err
	// tmpDir, err := os.MkdirTemp(dir, pattern)
	// if err != nil {
	// 	return "", err
	// }
	// tmpDir, err = filepath.EvalSymlinks(tmpDir)
	// if err != nil {
	// 	return "", fmt.Errorf("error evaluating symlink: %w", err)
	// }
	// return tmpDir, nil
}

func (r *InstallationReconciler) finalize(ctx context.Context, install zarfdevv1beta1.Installation) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	packager.Remove(install.Status.PackageName)

	// Remove our finalizer from the list and update it
	controllerutil.RemoveFinalizer(&install, zarfdevv1beta1.ZarfFinalizer)
	if err := r.Update(ctx, &install, client.FieldOwner(r.statusManager)); err != nil {
		l.Error(err, "Error removing finalizer from installation")
		return ctrl.Result{}, err
	}

	// Stop reconciliation as the object is being deleted
	return ctrl.Result{}, nil
}
