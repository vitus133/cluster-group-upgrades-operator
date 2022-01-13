/*
Copyright 2021.

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
	"fmt"
	"text/template"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type templateData struct {
	Cluster                 string
	ResourceName            string
	PlatformImage           string
	Operators               operatorsData
	PrecachingWorkloadImage string
	PrecachingJobTimeout    uint64
}

type operatorsData struct {
	Indexes             []string
	PackagesAndChannels []string
}

type resourceTemplate struct {
	// Must always correspond the Action or View resource name
	resourceName string
	// Template text
	template string
}

var precacheDependenciesCreateTemplates = []resourceTemplate{
	{"precache-ns-create", mngClusterActCreatePrecachingNS},
	{"precache-spec-cm-create", mngClusterActCreatePrecachingSpecCM},
	{"precache-sa-create", mngClusterActCreateServiceAcct},
	{"precache-crb-create", mngClusterActCreateClusterRoleBinding},
}

var precacheDependenciesViewTemplates = []resourceTemplate{
	{"view-precache-spec-configmap", mngClusterViewConfigMap},
	{"view-precache-service-acct", mngClusterViewServiceAcct},
	{"view-precache-cluster-role-binding", mngClusterViewClusterRoleBinding},
}

var precacheCreateTemplates = []resourceTemplate{
	{"precache-job-create", mngClusterActCreateJob},
	{"view-precache-job", mngClusterViewJob},
}

var precacheDeleteTemplates = []resourceTemplate{
	{"precache-ns-delete", mngClusterActDeletePrecachingNS},
}

var precacheNSViewTemplates = []resourceTemplate{
	{"view-precache-namespace", mngClusterViewNamespace},
}

var allPossibleClusterViewsForDelete = []resourceTemplate{
	{"view-precache-namespace", ""},
	{"view-precache-job", ""},
	{"view-precache-spec-configmap", ""},
	{"view-precache-service-acct", ""},
	{"view-precache-cluster-role-binding", ""},
}

// createResourceFromTemplate: managedclusteraction or managedclusterview
//      resources from templates using dynamic client
// returns:   error
func (r *ClusterGroupUpgradeReconciler) createResourcesFromTemplates(
	ctx context.Context, data *templateData, templates []resourceTemplate) error {

	config := ctrl.GetConfigOrDie()
	dynamic := dynamic.NewForConfigOrDie(config)

	for _, item := range templates {
		r.Log.Info("[createResourcesFromTemplates]", "cluster", data.Cluster, "template", item.resourceName)
		obj := &unstructured.Unstructured{}
		w, err := r.renderYamlTemplate(item.resourceName, item.template, *data)
		if err != nil {
			return err
		}

		// decode YAML into unstructured.Unstructured
		dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
		_, gvk, err := dec.Decode(w.Bytes(), nil, obj)
		if err != nil {
			return err
		}
		// Map GVK to GVR with discovery client
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return err
		}
		mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		// Build resource
		resource := schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: mapping.Resource.Resource,
		}
		_, err = dynamic.Resource(resource).Namespace(data.Cluster).Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			if errors.IsAlreadyExists(err) {
				r.Log.Info("[createResourcesFromTemplates] Already exists",
					"cluster", data.Cluster, "template", item.resourceName)
				return nil
			}
			return err
		}
	}
	return nil
}

// deleteManagedClusterViewResource: deletes view by name and namespace
// returns: error
func (r *ClusterGroupUpgradeReconciler) deleteManagedClusterViewResource(
	ctx context.Context, name string, namespace string) error {

	config := ctrl.GetConfigOrDie()
	dynamic := dynamic.NewForConfigOrDie(config)
	resourceID := schema.GroupVersionResource{
		Group:    "view.open-cluster-management.io",
		Version:  "v1beta1",
		Resource: "managedclusterviews",
	}
	err := dynamic.Resource(resourceID).Namespace(namespace).Delete(
		ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	return nil
}

// isViewChildResourceAvailable - checks the view underlying resource
//      is available and being watched
// returns:   available (bool)
//            error
func (r *ClusterGroupUpgradeReconciler) isViewChildResourceAvailable(
	ctx context.Context, name string, namespace string) (bool, error) {

	view := &unstructured.Unstructured{}
	var status, message string

	view.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "view.open-cluster-management.io",
		Kind:    "ManagedClusterView",
		Version: "v1beta1",
	})
	err := r.Client.Get(ctx, client.ObjectKey{
		Name:      name,
		Namespace: namespace,
	}, view)
	if err != nil {
		return false, err
	}
	// Looking for this:
	// status:
	// 	conditions:
	// 	- lastTransitionTime: ...
	// 		status: "True"
	// 		type: Processing
	viewConditions, exists, err := unstructured.NestedSlice(
		view.Object, "status", "conditions")
	if !exists {
		message = "[isViewChildResourceAvailable] no ManagedClusterView conditions found"
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, condition := range viewConditions {
		if condition.(map[string]interface{})["type"] == "Processing" {
			status = condition.(map[string]interface{})["status"].(string)
			message = condition.(map[string]interface{})["message"].(string)
			break
		}
	}
	r.Log.Info("[isViewChildResourceAvailable]", "name", name, "namespace",
		namespace, "status", status, "message", message)
	if status != "True" {
		return false, nil
	}
	return true, nil
}

// renderYamlTemplate: renders a single resource from the yaml template
//            resourceName - resource name
//            templateBody - template body
// returns:   bytes.Buffer rendered template
//            error
func (r *ClusterGroupUpgradeReconciler) renderYamlTemplate(
	resourceName string,
	templateBody string,
	data templateData) (*bytes.Buffer, error) {

	w := new(bytes.Buffer)
	template, err := template.New(resourceName).Parse(commonTemplates + templateBody)
	if err != nil {
		return w, fmt.Errorf("failed to parse template %s: %v", resourceName, err)
	}
	data.ResourceName = resourceName
	err = template.Execute(w, data)
	if err != nil {
		return w, fmt.Errorf("failed to render template %s: %v", resourceName, err)
	}
	return w, nil
}

const commonTemplates string = `
{{ define "actionGVK" }}
apiVersion: action.open-cluster-management.io/v1beta1
kind: ManagedClusterAction
{{ end }}

{{ define "viewGVK" }}
apiVersion: view.open-cluster-management.io/v1beta1
kind: ManagedClusterView
{{ end }}

{{ define "metadata"}}
metadata:
  name: {{ .ResourceName }}
  namespace: {{ .Cluster }}
{{ end }}
`

const mngClusterActCreatePrecachingNS string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Create
  kube:
    resource: namespace
    template:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: pre-cache
        annotations:
          workload.openshift.io/allowed: management
`

const mngClusterActCreatePrecachingSpecCM string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Create
  kube:
    resource: configmap
    template:
      apiVersion: v1
      data:
        operators.indexes: |{{ range .Operators.Indexes }}
          {{ . }} {{ end }}
        operators.packagesAndChannels: |{{ range .Operators.PackagesAndChannels }} 
          {{ . }} {{ end }}
        platform.image: {{ .PlatformImage }}
      kind: ConfigMap
      metadata:
        name: pre-cache-spec
        namespace: pre-cache
`

const mngClusterActCreateServiceAcct string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Create
  kube:
    resource: serviceaccount
    template:
      apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: pre-cache-agent
        namespace: pre-cache
`

const mngClusterActCreateClusterRoleBinding string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Create
  kube:
    resource: clusterrolebinding
    template:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRoleBinding
      metadata:
        name: pre-cache-crb
      roleRef:
        apiGroup: rbac.authorization.k8s.io
        kind: ClusterRole
        name: cluster-admin
      subjects:
        - kind: ServiceAccount
          name: pre-cache-agent
          namespace: pre-cache
`

const mngClusterActCreateJob string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Create
  kube:
    resource: job
    namespace: pre-cache
    template:
      apiVersion: batch/v1
      kind: Job
      metadata:
        name: pre-cache
        namespace: pre-cache
      spec:
        activeDeadlineSeconds: {{ .PrecachingJobTimeout }}
        backoffLimit: 0
        template:
          metadata:
            name: pre-cache
          spec:
            containers:
            - args:
              - /opt/precache/precache.sh
              # - sleep inf
              command:
              - /bin/bash
              - -c
              env:
              - name: config_volume_path
                value: /etc/config
              image: {{ .PrecachingWorkloadImage }}
              name: pre-cache-container
              resources: {}
              securityContext:
                privileged: true
                runAsUser: 0
              terminationMessagePath: /dev/termination-log
              terminationMessagePolicy: File
              volumeMounts:
              - mountPath: /host
                name: host 
              - mountPath: /etc/config
                name: config-volume
                readOnly: true
            dnsPolicy: ClusterFirst
            restartPolicy: Never
            schedulerName: default-scheduler
            securityContext: {}
            serviceAccountName: pre-cache-agent
            priorityClassName: system-cluster-critical
            volumes:
            - configMap:
                defaultMode: 420
                name: pre-cache-spec
              name: config-volume
            - hostPath:
                path: /
                type: Directory
              name: host

`

const mngClusterViewJob string = `
{{ template "viewGVK"}}
{{ template "metadata" . }}
spec:
  scope:
    resource: jobs
    name: pre-cache
    namespace: pre-cache
    updateIntervalSeconds: 300
`

const mngClusterViewConfigMap string = `
{{ template "viewGVK"}}
{{ template "metadata" . }}
spec:
  scope:
    resource: configmap
    name: pre-cache-spec
    namespace: pre-cache
    updateIntervalSeconds: 300
`

const mngClusterViewServiceAcct string = `
{{ template "viewGVK"}}
{{ template "metadata" . }}
spec:
  scope:
    resource: serviceaccounts
    name: pre-cache-agent
    namespace: pre-cache
    updateIntervalSeconds: 300
`

const mngClusterViewClusterRoleBinding string = `
{{ template "viewGVK"}}
{{ template "metadata" . }}
spec:
  scope:
    resource: clusterrolebinding
    name: pre-cache-crb
    updateIntervalSeconds: 300
`

const mngClusterViewNamespace string = `
{{ template "viewGVK"}}
{{ template "metadata" . }}
spec:
  scope:
    resource: namespaces
    name: pre-cache
    updateIntervalSeconds: 300
`

const mngClusterActDeletePrecachingNS string = `
{{ template "actionGVK"}}
{{ template "metadata" . }}
spec:
  actionType: Delete
  kube:
    resource: namespace
    name: pre-cache
`
