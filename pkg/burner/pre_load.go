// Copyright 2022 The Kube-burner Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package burner

import (
	"context"
	"fmt"
	"time"

	"maps"

	"github.com/kube-burner/kube-burner/pkg/util"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const preLoadNs = "preload-kube-burner"

// NestedPod represents a pod nested in a higher level object such as deployment or a daemonset
type NestedPod struct {
	// Spec represents the object spec
	Spec struct {
		Template struct {
			corev1.PodSpec `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type VMI struct {
	Spec struct {
		Volumes []struct {
			ContainerDisk struct {
				Image string `yaml:"image"`
			} `yaml:"containerDisk"`
		} `yaml:"volumes"`
	} `yaml:"spec"`
}

type NestedVM struct {
	Spec struct {
		Template struct {
			Spec struct {
				Volumes []struct {
					ContainerDisk struct {
						Image string `yaml:"image"`
					} `yaml:"containerDisk"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func preLoadImages(job Executor, clientSet kubernetes.Interface) error {
	log.Info("Pre-load: images from job ", job.Name)
	imageList, err := getJobImages(job)
	if err != nil {
		return fmt.Errorf("pre-load: %v", err)
	}
	if len(imageList) == 0 {
		log.Infof("No images found to pre-load, continuing")
		return nil
	}
	err = createDSs(clientSet, imageList, job.NamespaceLabels, job.NamespaceAnnotations, job.PreLoadNodeLabels)
	if err != nil {
		return fmt.Errorf("pre-load: %v", err)
	}
	log.Infof("Pre-load: Sleeping for %v", job.PreLoadPeriod)
	time.Sleep(job.PreLoadPeriod)
	// 5 minutes should be more than enough to cleanup this namespace
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	util.CleanupNamespaces(ctx, clientSet, "kube-burner-preload=true")
	return nil
}

func getJobImages(job Executor) ([]string, error) {
	var imageList []string
	var unstructuredObject unstructured.Unstructured
	for _, object := range job.objects {
		renderedObj, err := util.RenderTemplate(object.objectSpec, object.InputVars, util.MissingKeyZero, job.functionTemplates)
		if err != nil {
			return imageList, err
		}
		yamlToUnstructured(object.ObjectTemplate, renderedObj, &unstructuredObject)
		switch unstructuredObject.GetKind() {
		case Deployment, DaemonSet, ReplicaSet, Job, StatefulSet:
			var pod NestedPod
			runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObject.UnstructuredContent(), &pod)
			for _, i := range pod.Spec.Template.Containers {
				imageList = append(imageList, i.Image)
			}
		case Pod:
			var pod corev1.Pod
			runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObject.UnstructuredContent(), &pod)
			for _, i := range pod.Spec.Containers {
				if i.Image != "" {
					imageList = append(imageList, i.Image)
				}
			}
		case VirtualMachineInstance:
			var vmi VMI
			yaml.Unmarshal(renderedObj, &vmi)
			for _, volume := range vmi.Spec.Volumes {
				if volume.ContainerDisk.Image != "" {
					imageList = append(imageList, volume.ContainerDisk.Image)
				}
			}
		case VirtualMachine, VirtualMachineInstanceReplicaSet:
			var nestedVM NestedVM
			yaml.Unmarshal(renderedObj, &nestedVM)
			for _, volume := range nestedVM.Spec.Template.Spec.Volumes {
				if volume.ContainerDisk.Image != "" {
					imageList = append(imageList, volume.ContainerDisk.Image)
				}
			}
		}
	}
	return imageList, nil
}

func createDSs(clientSet kubernetes.Interface, imageList []string, namespaceLabels map[string]string, namespaceAnnotations map[string]string, nodeSelectorLabels map[string]string) error {
	nsLabels := map[string]string{
		"kube-burner-preload": "true",
	}
	nsAnnotations := make(map[string]string)
	maps.Copy(nsLabels, namespaceLabels)
	maps.Copy(nsAnnotations, namespaceAnnotations)
	if err := util.CreateNamespace(clientSet, preLoadNs, nsLabels, nsAnnotations); err != nil {
		log.Fatal(err)
	}
	dsName := "preload"
	ds := appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       string(DaemonSet),
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dsName,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": dsName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": dsName},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To[int64](0),
					InitContainers:                []corev1.Container{},
					// Only Always restart policy is supported
					Containers: []corev1.Container{
						{
							Name:            "sleep",
							Image:           "registry.k8s.io/pause:3.1",
							ImagePullPolicy: corev1.PullAlways,
						},
					},
					NodeSelector: nodeSelectorLabels,
				},
			},
		},
	}

	// Add the list of containers using images
	for i, image := range imageList {
		container := corev1.Container{
			Name:            fmt.Sprintf("container-%d", i),
			ImagePullPolicy: corev1.PullAlways,
			Image:           image,
			Command:         []string{"echo", fmt.Sprintf("init container-%d completed", i)},
		}
		ds.Spec.Template.Spec.InitContainers = append(ds.Spec.Template.Spec.InitContainers, container)
	}

	log.Infof("Pre-load: Creating DaemonSet using images %v in namespace %s", imageList, preLoadNs)
	_, err := clientSet.AppsV1().DaemonSets(preLoadNs).Create(context.TODO(), &ds, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	return nil
}
