// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"
	json "encoding/json"
	"fmt"

	v1 "github.com/openshift/api/operator/v1"
	operatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeOpenShiftControllerManagers implements OpenShiftControllerManagerInterface
type FakeOpenShiftControllerManagers struct {
	Fake *FakeOperatorV1
}

var openshiftcontrollermanagersResource = v1.SchemeGroupVersion.WithResource("openshiftcontrollermanagers")

var openshiftcontrollermanagersKind = v1.SchemeGroupVersion.WithKind("OpenShiftControllerManager")

// Get takes name of the openShiftControllerManager, and returns the corresponding openShiftControllerManager object, and an error if there is any.
func (c *FakeOpenShiftControllerManagers) Get(ctx context.Context, name string, options metav1.GetOptions) (result *v1.OpenShiftControllerManager, err error) {
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootGetActionWithOptions(openshiftcontrollermanagersResource, name, options), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// List takes label and field selectors, and returns the list of OpenShiftControllerManagers that match those selectors.
func (c *FakeOpenShiftControllerManagers) List(ctx context.Context, opts metav1.ListOptions) (result *v1.OpenShiftControllerManagerList, err error) {
	emptyResult := &v1.OpenShiftControllerManagerList{}
	obj, err := c.Fake.
		Invokes(testing.NewRootListActionWithOptions(openshiftcontrollermanagersResource, openshiftcontrollermanagersKind, opts), emptyResult)
	if obj == nil {
		return emptyResult, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1.OpenShiftControllerManagerList{ListMeta: obj.(*v1.OpenShiftControllerManagerList).ListMeta}
	for _, item := range obj.(*v1.OpenShiftControllerManagerList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested openShiftControllerManagers.
func (c *FakeOpenShiftControllerManagers) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewRootWatchActionWithOptions(openshiftcontrollermanagersResource, opts))
}

// Create takes the representation of a openShiftControllerManager and creates it.  Returns the server's representation of the openShiftControllerManager, and an error, if there is any.
func (c *FakeOpenShiftControllerManagers) Create(ctx context.Context, openShiftControllerManager *v1.OpenShiftControllerManager, opts metav1.CreateOptions) (result *v1.OpenShiftControllerManager, err error) {
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootCreateActionWithOptions(openshiftcontrollermanagersResource, openShiftControllerManager, opts), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// Update takes the representation of a openShiftControllerManager and updates it. Returns the server's representation of the openShiftControllerManager, and an error, if there is any.
func (c *FakeOpenShiftControllerManagers) Update(ctx context.Context, openShiftControllerManager *v1.OpenShiftControllerManager, opts metav1.UpdateOptions) (result *v1.OpenShiftControllerManager, err error) {
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateActionWithOptions(openshiftcontrollermanagersResource, openShiftControllerManager, opts), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeOpenShiftControllerManagers) UpdateStatus(ctx context.Context, openShiftControllerManager *v1.OpenShiftControllerManager, opts metav1.UpdateOptions) (result *v1.OpenShiftControllerManager, err error) {
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateSubresourceActionWithOptions(openshiftcontrollermanagersResource, "status", openShiftControllerManager, opts), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// Delete takes name of the openShiftControllerManager and deletes it. Returns an error if one occurs.
func (c *FakeOpenShiftControllerManagers) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewRootDeleteActionWithOptions(openshiftcontrollermanagersResource, name, opts), &v1.OpenShiftControllerManager{})
	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeOpenShiftControllerManagers) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	action := testing.NewRootDeleteCollectionActionWithOptions(openshiftcontrollermanagersResource, opts, listOpts)

	_, err := c.Fake.Invokes(action, &v1.OpenShiftControllerManagerList{})
	return err
}

// Patch applies the patch and returns the patched openShiftControllerManager.
func (c *FakeOpenShiftControllerManagers) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.OpenShiftControllerManager, err error) {
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceActionWithOptions(openshiftcontrollermanagersResource, name, pt, data, opts, subresources...), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// Apply takes the given apply declarative configuration, applies it and returns the applied openShiftControllerManager.
func (c *FakeOpenShiftControllerManagers) Apply(ctx context.Context, openShiftControllerManager *operatorv1.OpenShiftControllerManagerApplyConfiguration, opts metav1.ApplyOptions) (result *v1.OpenShiftControllerManager, err error) {
	if openShiftControllerManager == nil {
		return nil, fmt.Errorf("openShiftControllerManager provided to Apply must not be nil")
	}
	data, err := json.Marshal(openShiftControllerManager)
	if err != nil {
		return nil, err
	}
	name := openShiftControllerManager.Name
	if name == nil {
		return nil, fmt.Errorf("openShiftControllerManager.Name must be provided to Apply")
	}
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceActionWithOptions(openshiftcontrollermanagersResource, *name, types.ApplyPatchType, data, opts.ToPatchOptions()), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}

// ApplyStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
func (c *FakeOpenShiftControllerManagers) ApplyStatus(ctx context.Context, openShiftControllerManager *operatorv1.OpenShiftControllerManagerApplyConfiguration, opts metav1.ApplyOptions) (result *v1.OpenShiftControllerManager, err error) {
	if openShiftControllerManager == nil {
		return nil, fmt.Errorf("openShiftControllerManager provided to Apply must not be nil")
	}
	data, err := json.Marshal(openShiftControllerManager)
	if err != nil {
		return nil, err
	}
	name := openShiftControllerManager.Name
	if name == nil {
		return nil, fmt.Errorf("openShiftControllerManager.Name must be provided to Apply")
	}
	emptyResult := &v1.OpenShiftControllerManager{}
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceActionWithOptions(openshiftcontrollermanagersResource, *name, types.ApplyPatchType, data, opts.ToPatchOptions(), "status"), emptyResult)
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.OpenShiftControllerManager), err
}
