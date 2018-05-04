package ipamshared

import (
	"bytes"
	"fmt"
	"text/template"

	log "github.com/sirupsen/logrus"

	"github.com/Nexinto/go-ipam"

	"k8s.io/client-go/kubernetes"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1 "github.com/Nexinto/k8s-ipam/pkg/apis/ipam.nexinto.com/v1"
	ipamclientset "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned"
)

type SharedController struct {
	Kubernetes   kubernetes.Interface
	IpamClient   ipamclientset.Interface
	Ipam         ipam.Ipam
	Tag          string
	NameTemplate *template.Template
	IpamName     string
}

// Create the name for an ipadress object
func (c *SharedController) NameFor(a *ipamv1.IpAddress) string {
	var buffer bytes.Buffer

	if a.Spec.Name != "" {
		return a.Spec.Name
	}

	err := c.NameTemplate.Execute(&buffer, struct {
		Tag       string
		Namespace string
		Name      string
	}{
		Tag:       c.Tag,
		Namespace: a.Namespace,
		Name:      a.Name,
	})

	if err != nil {
		panic(err)
	}

	return buffer.String()
}

// Create an event for an object.
func (c *SharedController) MakeEvent(o metav1.Object, message string, warn bool) error {
	var t string
	if warn {
		t = "Warning"
	} else {
		t = "Normal"
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: o.GetName(),
		},
		InvolvedObject: corev1.ObjectReference{
			Name:            o.GetName(),
			Namespace:       o.GetNamespace(),
			APIVersion:      "v1",
			UID:             o.GetUID(),
			Kind:            "IpAddress",
			ResourceVersion: o.GetResourceVersion(),
		},
		Message:        message,
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Type:           t,
	}

	_, err := c.Kubernetes.CoreV1().Events(o.GetNamespace()).Create(event)
	return err
}

// Create a Warning Event for the object and also return it as an error.
func (c *SharedController) LogEventAndFail(o metav1.Object, message string) error {
	log.Error(message)
	_ = c.MakeEvent(o, message, true)
	return fmt.Errorf(message)
}

func (c *SharedController) IpAddressCreatedOrUpdated(a *ipamv1.IpAddress) error {
	log.Debugf("processing address %s-%s", a.Namespace, a.Name)

	oname := c.NameFor(a)

	if a.Status.Address == "" {

		log.Debugf("address is unassigned")

		var ip string
		var err error

		if a.Spec.Ref == "" {
			ip, err = c.Ipam.Assign(oname)
			if err != nil {
				return c.LogEventAndFail(a, fmt.Sprintf("could not assign new address for '%s-%s': %s", a.Namespace, a.Name, err.Error()))
			}
		} else {
			addresses, err := c.Ipam.Search(a.Spec.Ref, true)
			if err != nil {
				return c.LogEventAndFail(a, fmt.Sprintf("error searching for address matching '%s' for '%s-%s': %s", a.Spec.Ref, a.Namespace, a.Name, err.Error()))
			}

			if len(addresses) == 0 {
				return c.LogEventAndFail(a, fmt.Sprintf("did not find address matching '%s' for '%s-%s'", a.Spec.Ref, a.Namespace, a.Name))
			} else if len(addresses) > 1 {
				return c.LogEventAndFail(a, fmt.Sprintf("found %d addresses matching '%s' for '%s-%s', need exactly one", len(addresses), a.Spec.Ref, a.Namespace, a.Name))
			}

			ip = addresses[0]
		}

		log.Infof("assigned %s for '%s-%s'", ip, a.Namespace, a.Name)

		a2 := a.DeepCopy()
		a2.Status.Address = ip
		a2.Status.Name = oname
		a2.Status.Provider = c.IpamName

		_, err = c.IpamClient.IpamV1().IpAddresses(a.Namespace).Update(a2)

		if err != nil {
			return c.LogEventAndFail(a, fmt.Sprintf("assigned address %s for '%s-%s', but could not update object: %s", ip, a.Namespace, a.Name, err.Error()))
		}
	} else {

		// TODO: check our address still exists?

		log.Debug("nothing to do")
	}

	return nil
}

func (c *SharedController) IpAddressDeleted(a *ipamv1.IpAddress) error {

	// TODO: What to do with addresses that have other addresses referring to them? Finalizer? Cascading delete?

	log.Debugf("processing deleted address %s-%s", a.Namespace, a.Name)

	if a.Status.Provider != "" && a.Status.Provider != c.IpamName {
		log.Debugf("ignoring address, created by provider '%s'", a.Status.Provider)
		return nil
	}

	if a.Status.Address == "" {
		// object was never assigned
		log.Debug("nothing to do: address was never assigned")
		return nil
	}

	if a.Spec.Ref != "" {
		// a reference
		log.Debug("nothing to do: address was a reference")
		return nil
	}

	err := c.Ipam.Unassign(a.Status.Address)
	if err != nil {
		return c.LogEventAndFail(a, fmt.Sprintf("could not unassign address %s for '%s-%s' from IPAM: %s", a.Status.Address, a.Namespace, a.Name, err.Error()))
	}

	log.Debugf("address %s for '%s-%s' successfully unassigned", a.Status.Address, a.Namespace, a.Name)

	return nil
}
