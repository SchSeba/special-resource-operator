package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	machinev1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	"github.com/openshift/special-resource-operator/pkg/clients"
	"github.com/openshift/special-resource-operator/pkg/utils"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

//go:generate mockgen -source=cluster.go -package=cluster -destination=mock_cluster_api.go

type Cluster interface {
	Version(context.Context) (string, string, error)
	OSImageURL(context.Context) (string, error)
	OperatingSystem(*corev1.NodeList) (string, string, string, error)
	GetDTKImages(context.Context) ([]string, error)
}

func NewCluster(clients clients.ClientsInterface) Cluster {
	return &cluster{
		log:     zap.New(zap.UseDevMode(true)).WithName(utils.Print("cache", utils.Brown)),
		clients: clients,
	}
}

type cluster struct {
	log     logr.Logger
	clients clients.ClientsInterface
}

// GetDTKImages returns URLs to DTK images obtained from cluster's DTK ImageStream
func (c *cluster) GetDTKImages(ctx context.Context) ([]string, error) {
	is := imagev1.ImageStream{}

	err := c.clients.Get(ctx,
		types.NamespacedName{Namespace: "openshift", Name: "driver-toolkit"},
		&is)
	if err != nil {
		return nil, fmt.Errorf("could not obtain openshift/driver-toolkit ImageStream: %w", err)
	}

	type tagRef struct {
		ref     string
		created time.Time
	}

	trs := []tagRef{}
	for _, tag := range is.Status.Tags {
		if tag.Tag == "latest" {
			for _, t := range tag.Items {
				trs = append(trs, tagRef{ref: t.DockerImageReference, created: t.Created.Time})
			}
		}
	}

	sort.Slice(trs, func(i, j int) bool {
		return trs[i].created.After(trs[j].created)
	})

	refs := make([]string, 0, len(trs))
	for _, tr := range trs {
		refs = append(refs, tr.ref)
	}

	return refs, nil
}

func (c *cluster) Version(ctx context.Context) (string, string, error) {

	available, err := c.clusterVersionAvailable()
	if err != nil {
		return "", "", err
	}
	if !available {
		return "", "", nil
	}

	version, err := c.clients.ClusterVersionGet(ctx, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("ConfigClient unable to get ClusterVersions: %w", err)
	}

	var majorMinor string
	for _, condition := range version.Status.History {
		if condition.State != "Completed" {
			continue
		}

		s := strings.Split(condition.Version, ".")

		if len(s) > 1 {
			majorMinor = s[0] + "." + s[1]
		} else {
			majorMinor = s[0]
		}

		return condition.Version, majorMinor, nil
	}

	return "", "", errors.New("Undefined Cluster Version")
}

func (c *cluster) OSImageURL(ctx context.Context) (string, error) {

	machineConfigAvailable, err := c.clients.HasResource(machinev1.SchemeGroupVersion.WithResource("machineconfigs"))
	if err != nil {
		return "", fmt.Errorf("Error discovering machineconfig API resource: %w", err)
	}
	if !machineConfigAvailable {
		c.log.Info("Warning: Could not find machineconfig API resource. Can be ignored on vanilla k8s.")
		return "", nil
	}

	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")

	namespacedName := types.NamespacedName{Namespace: "openshift-machine-config-operator", Name: "machine-config-osimageurl"}
	err = c.clients.Get(ctx, namespacedName, cm)
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("ConfigMap machine-config-osimageurl -n  openshift-machine-config-operator not found: %w", err)
	}

	osImageURL, found, err := unstructured.NestedString(cm.Object, "data", "osImageURL")
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("osImageURL not found")
	}

	return osImageURL, nil
}

// OperatingSystem returns the OS version in the following form: rhelx, rhelx.y, x.y
// Assumes all nodes have the same OS.
func (c *cluster) OperatingSystem(nodeList *corev1.NodeList) (string, string, string, error) {
	var nodeOS string
	var nodeOSMajor string
	var err error
	for _, node := range nodeList.Items {
		_, nodeOS, nodeOSMajor, err = utils.ParseOSInfo(node.Status.NodeInfo.OSImage)
		if err != nil {
			return "", "", "", fmt.Errorf("unable to get node %s OS image info: %w", node.Name, err)
		}
	}
	return "rhel" + nodeOSMajor, "rhel" + nodeOS, nodeOS, nil
}

func (c *cluster) clusterVersionAvailable() (bool, error) {

	clusterVersionAvailable, err := c.clients.HasResource(configv1.SchemeGroupVersion.WithResource("clusterversions"))
	if err != nil {
		return false, err
	}
	if !clusterVersionAvailable {
		c.log.Info("Warning: ClusterVersion API resource not available. Can be ignored on vanilla k8s.")
		return false, nil
	}
	return true, nil
}
