// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployment

import (
	"log"

	"github.com/kubernetes/dashboard/src/app/backend/api"
	"github.com/kubernetes/dashboard/src/app/backend/integration/metric/heapster"
	"github.com/kubernetes/dashboard/src/app/backend/resource/common"
	"github.com/kubernetes/dashboard/src/app/backend/resource/dataselect"
	"github.com/kubernetes/dashboard/src/app/backend/resource/event"
	"github.com/kubernetes/dashboard/src/app/backend/resource/metric"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

// ReplicationSetList contains a list of Deployments in the cluster.
type DeploymentList struct {
	ListMeta api.ListMeta `json:"listMeta"`

	// Unordered list of Deployments.
	Deployments       []Deployment    `json:"deployments"`
	CumulativeMetrics []metric.Metric `json:"cumulativeMetrics"`
}

// Deployment is a presentation layer view of Kubernetes Deployment resource. This means
// it is Deployment plus additional augmented data we can get from other sources
// (like services that target the same pods).
type Deployment struct {
	ObjectMeta api.ObjectMeta `json:"objectMeta"`
	TypeMeta   api.TypeMeta   `json:"typeMeta"`

	// Aggregate information about pods belonging to this Deployment.
	Pods common.PodInfo `json:"pods"`

	// Container images of the Deployment.
	ContainerImages []string `json:"containerImages"`
}

// GetDeploymentList returns a list of all Deployments in the cluster.
func GetDeploymentList(client client.Interface, nsQuery *common.NamespaceQuery,
	dsQuery *dataselect.DataSelectQuery, heapsterClient *heapster.HeapsterClient) (*DeploymentList, error) {
	log.Print("Getting list of all deployments in the cluster")

	channels := &common.ResourceChannels{
		DeploymentList: common.GetDeploymentListChannel(client, nsQuery, 1),
		PodList:        common.GetPodListChannel(client, nsQuery, 1),
		EventList:      common.GetEventListChannel(client, nsQuery, 1),
		ReplicaSetList: common.GetReplicaSetListChannel(client, nsQuery, 1),
	}

	return GetDeploymentListFromChannels(channels, dsQuery, heapsterClient)
}

// GetDeploymentList returns a list of all Deployments in the cluster
// reading required resource list once from the channels.
func GetDeploymentListFromChannels(channels *common.ResourceChannels,
	dsQuery *dataselect.DataSelectQuery, heapsterClient *heapster.HeapsterClient) (*DeploymentList, error) {

	deployments := <-channels.DeploymentList.List
	if err := <-channels.DeploymentList.Error; err != nil {
		statusErr, ok := err.(*k8serrors.StatusError)
		if ok && statusErr.ErrStatus.Reason == "NotFound" {
			// NotFound - this means that the server does not support Deployment objects, which
			// is fine.
			emptyList := &DeploymentList{
				Deployments: make([]Deployment, 0),
			}
			return emptyList, nil
		}
		return nil, err
	}

	pods := <-channels.PodList.List
	if err := <-channels.PodList.Error; err != nil {
		return nil, err
	}

	events := <-channels.EventList.List
	if err := <-channels.EventList.Error; err != nil {
		return nil, err
	}

	rs := <-channels.ReplicaSetList.List
	if err := <-channels.ReplicaSetList.Error; err != nil {
		return nil, err
	}

	return CreateDeploymentList(deployments.Items, pods.Items, events.Items, rs.Items, dsQuery, heapsterClient), nil
}

// CreateDeploymentList returns a list of all Deployment model objects in the cluster, based on all
// Kubernetes Deployment API objects.
func CreateDeploymentList(deployments []extensions.Deployment, pods []v1.Pod, events []v1.Event,
	rs []extensions.ReplicaSet, dsQuery *dataselect.DataSelectQuery,
	heapsterClient *heapster.HeapsterClient) *DeploymentList {

	deploymentList := &DeploymentList{
		Deployments: make([]Deployment, 0),
		ListMeta:    api.ListMeta{TotalItems: len(deployments)},
	}

	cachedResources := &dataselect.CachedResources{
		Pods: pods,
	}
	deploymentCells, metricPromises, filteredTotal := dataselect.GenericDataSelectWithFilterAndMetrics(toCells(deployments), dsQuery, cachedResources, heapsterClient)
	deployments = fromCells(deploymentCells)
	deploymentList.ListMeta = api.ListMeta{TotalItems: filteredTotal}

	for _, deployment := range deployments {
		matchingPods := common.FilterDeploymentPodsByOwnerReference(deployment, rs, pods)
		podInfo := common.GetPodInfo(deployment.Status.Replicas, *deployment.Spec.Replicas,
			matchingPods)
		podInfo.Warnings = event.GetPodsEventWarnings(events, matchingPods)

		deploymentList.Deployments = append(deploymentList.Deployments,
			Deployment{
				ObjectMeta:      api.NewObjectMeta(deployment.ObjectMeta),
				TypeMeta:        api.NewTypeMeta(api.ResourceKindDeployment),
				ContainerImages: common.GetContainerImages(&deployment.Spec.Template.Spec),
				Pods:            podInfo,
			})
	}

	cumulativeMetrics, err := metricPromises.GetMetrics()
	deploymentList.CumulativeMetrics = cumulativeMetrics
	if err != nil {
		deploymentList.CumulativeMetrics = make([]metric.Metric, 0)
	}

	return deploymentList
}
