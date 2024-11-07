package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	zv1 "github.com/zalando-incubator/es-operator/pkg/apis/zalando.org/v1"

	v1 "k8s.io/api/core/v1"

	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultWaitTimeout = 15 * time.Minute
)

var (
	edsPodSpec = func(nodeGroup, version, configMap string) v1.PodSpec {
		return v1.PodSpec{
			SecurityContext: &v1.PodSecurityContext{
				RunAsUser:  pint64(1000),
				RunAsGroup: pint64(0),
				FSGroup:    pint64(0),
			},
			Containers: []v1.Container{
				{
					Name: "elasticsearch",
					// gets replaced with desired version
					Image: fmt.Sprintf("docker.elastic.co/elasticsearch/elasticsearch:%s", version),
					Ports: []v1.ContainerPort{
						{
							ContainerPort: 9200,
						},
						{
							ContainerPort: 9300,
						},
					},
					Env: []v1.EnvVar{
						{Name: "ES_JAVA_OPTS", Value: "-Xms356m -Xmx356m"},
						{Name: "node.roles", Value: "data"},
						{Name: "node.attr.group", Value: nodeGroup},
					},
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
							v1.ResourceCPU:    resource.MustParse("100m"),
						},
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
							v1.ResourceCPU:    resource.MustParse("100m"),
						},
					},
					ReadinessProbe: &v1.Probe{
						InitialDelaySeconds: 15,
						ProbeHandler: v1.ProbeHandler{
							HTTPGet: &v1.HTTPGetAction{
								Path:   "/_cluster/health?local=true",
								Port:   intstr.FromInt(9200),
								Scheme: v1.URISchemeHTTP,
							},
						},
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/usr/share/elasticsearch/data",
						},
						{
							Name:      "config",
							MountPath: "/usr/share/elasticsearch/config/elasticsearch.yml",
							SubPath:   "elasticsearch.yml",
						},
					},
				},
			},
			TerminationGracePeriodSeconds: pint64(5),
			Volumes: []v1.Volume{
				{
					Name: "data",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{
							Medium: v1.StorageMediumMemory,
						},
					},
				},
				{
					Name: "config",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: v1.LocalObjectReference{
								Name: configMap,
							},
							Items: []v1.KeyToPath{
								{
									Key:  "elasticsearch.yml",
									Path: "elasticsearch.yml",
								},
							},
						},
					},
				},
			},
		}
	}
	edsPodSpecCPULoadContainer = func(nodeGroup, version, configMap string) v1.PodSpec {
		podSpec := edsPodSpec(nodeGroup, version, configMap)
		podSpec.Containers = append(podSpec.Containers, v1.Container{
			Name: "stress-ng",
			// https://hub.docker.com/r/alexeiled/stress-ng/
			Image: "alexeiled/stress-ng",
			Args:  []string{"--cpu=1", "--cpu-load=10"},
			Resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("50Mi"),
					v1.ResourceCPU:    resource.MustParse("100m"),
				},
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("50Mi"),
					v1.ResourceCPU:    resource.MustParse("100m"),
				},
			},
		})
		return podSpec
	}
)

type awaiter struct {
	t           *testing.T
	description string
	timeout     time.Duration
	poll        func() (retry bool, err error)
}

func (a *awaiter) withTimeout(timeout time.Duration) *awaiter {
	a.timeout = timeout
	return a
}

func (a *awaiter) withPoll(poll func() (retry bool, err error)) *awaiter {
	a.poll = poll
	return a
}

func newAwaiter(t *testing.T, description string) *awaiter {
	return &awaiter{
		t:           t,
		description: description,
		timeout:     defaultWaitTimeout,
	}
}

func (a *awaiter) await() error {
	deadline := time.Now().Add(a.timeout)
	a.t.Logf("Waiting for %s until %s (UTC)...", a.description, deadline.Format("3:04PM"))
	for {
		retry, err := a.poll()
		if err != nil {
			a.t.Logf("%v", err)
			if retry && time.Now().Before(deadline) {
				time.Sleep(30 * time.Second)
				continue
			}
			return err
		}
		a.t.Logf("Finished waiting for %s", a.description)
		return nil
	}
}

func resourceCreated(t *testing.T, kind string, name string, k8sInterface interface{}) *awaiter {
	get := reflect.ValueOf(k8sInterface).MethodByName("Get")
	return newAwaiter(t, fmt.Sprintf("creation of %s %s", kind, name)).withPoll(func() (bool, error) {
		result := get.Call([]reflect.Value{
			reflect.ValueOf(context.Background()),
			reflect.ValueOf(name),
			reflect.ValueOf(metav1.GetOptions{}),
		})
		err := result[1].Interface()
		if err != nil {
			t.Logf("%v", err)
			return apiErrors.IsNotFound(err.(error)), err.(error)
		}
		return false, nil
	})
}

func waitForEDS(t *testing.T, name string) (*zv1.ElasticsearchDataSet, error) {
	err := resourceCreated(t, "eds", name, edsInterface()).await()
	if err != nil {
		return nil, err
	}
	return edsInterface().Get(context.Background(), name, metav1.GetOptions{})
}

func waitForStatefulSet(t *testing.T, name string) (*appsv1.StatefulSet, error) {
	err := resourceCreated(t, "sts", name, statefulSetInterface()).await()
	if err != nil {
		return nil, err
	}
	return statefulSetInterface().Get(context.Background(), name, metav1.GetOptions{})
}

func waitForService(t *testing.T, name string) (*v1.Service, error) {
	err := resourceCreated(t, "service", name, serviceInterface()).await()
	if err != nil {
		return nil, err
	}
	return serviceInterface().Get(context.Background(), name, metav1.GetOptions{})
}

type expectedStsStatus struct {
	replicas        *int32
	readyReplicas   *int32
	updatedReplicas *int32
}

func (expected expectedStsStatus) matches(sts *appsv1.StatefulSet) error {
	status := sts.Status
	if sts.Generation != sts.Status.ObservedGeneration {
		return fmt.Errorf("%s: observedGeneration %d != expected %d", sts.Name, status.ObservedGeneration, sts.Generation)
	}
	if expected.replicas != nil && status.Replicas != *expected.replicas {
		return fmt.Errorf("%s: replicas %d != expected %d", sts.Name, status.Replicas, *expected.replicas)
	}
	if expected.updatedReplicas != nil && status.UpdatedReplicas != *expected.updatedReplicas {
		return fmt.Errorf("%s: updatedReplicas %d != expected %d", sts.Name, status.UpdatedReplicas, *expected.updatedReplicas)
	}
	if expected.readyReplicas != nil && status.ReadyReplicas != *expected.readyReplicas {
		return fmt.Errorf("%s: readyReplicas %d != expected %d", sts.Name, status.ReadyReplicas, *expected.readyReplicas)
	}
	return nil
}

func waitForEDSCondition(t *testing.T, name string, conditions ...func(eds *zv1.ElasticsearchDataSet) error) error {
	return newAwaiter(t, fmt.Sprintf("eds %s to reach desired condition", name)).withPoll(func() (retry bool, err error) {
		eds, err := edsInterface().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, condition := range conditions {
			err := condition(eds)
			if err != nil {
				return true, err
			}
		}
		return true, nil
	}).await()
}

func waitForSTSCondition(t *testing.T, stsName string, conditions ...func(sts *appsv1.StatefulSet) error) error {
	return newAwaiter(t, fmt.Sprintf("sts %s to reach desired condition", stsName)).withPoll(func() (retry bool, err error) {
		sts, err := statefulSetInterface().Get(context.Background(), stsName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, condition := range conditions {
			err := condition(sts)
			if err != nil {
				return true, err
			}
		}
		return true, nil
	}).await()
}

func createEDS(name string, spec zv1.ElasticsearchDataSetSpec) error {
	myspec := spec.DeepCopy()
	for i, env := range myspec.Template.Spec.Containers[0].Env {
		if env.Name == "node.attr.group" {
			myspec.Template.Spec.Containers[0].Env[i].Value = name
			break
		}
	}
	eds := &zv1.ElasticsearchDataSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"es-operator.zalando.org/operator": operatorId,
			},
		},
		Spec: *myspec,
	}
	_, err := edsInterface().Create(context.Background(), eds, metav1.CreateOptions{})
	return err
}

func updateEDS(name string, eds *zv1.ElasticsearchDataSet) error {
	_, err := edsInterface().Update(context.Background(), eds, metav1.UpdateOptions{})
	return err
}

func deleteEDS(name string) error {
	err := edsInterface().Delete(context.Background(), name, metav1.DeleteOptions{GracePeriodSeconds: pint64(10)})
	return err
}

func pbool(b bool) *bool {
	return &b
}

func pint64(i int64) *int64 {
	return &i
}

func pint32(i int32) *int32 {
	return &i
}
