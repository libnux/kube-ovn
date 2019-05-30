package controller

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/alauda/kube-ovn/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1"
	netv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

func (c *Controller) enqueueAddNp(obj interface{}) {
	if !c.isLeader() {
		return
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	klog.V(3).Infof("enqueue add np %s", key)
	c.updateNpQueue.AddRateLimited(key)
}

func (c *Controller) enqueueDeleteNp(obj interface{}) {
	if !c.isLeader() {
		return
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	klog.V(3).Infof("enqueue delete np %s", key)
	c.deleteNpQueue.AddRateLimited(key)
}

func (c *Controller) enqueueUpdateNp(old, new interface{}) {
	if !c.isLeader() {
		return
	}
	oldNp := old.(*v1.NetworkPolicy)
	newNp := new.(*v1.NetworkPolicy)
	if !reflect.DeepEqual(oldNp.Spec, newNp.Spec) {
		var key string
		var err error
		if key, err = cache.MetaNamespaceKeyFunc(new); err != nil {
			utilruntime.HandleError(err)
			return
		}
		klog.V(3).Infof("enqueue update np %s", key)
		c.updateNpQueue.AddRateLimited(key)
	}
}

func (c *Controller) runUpdateNpWorker() {
	for c.processNextUpdateNpWorkItem() {
	}
}

func (c *Controller) runDeleteNpWorker() {
	for c.processNextDeleteNpWorkItem() {
	}
}

func (c *Controller) processNextUpdateNpWorkItem() bool {
	obj, shutdown := c.updateNpQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.updateNpQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.updateNpQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleUpdateNp(key); err != nil {
			c.updateNpQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.updateNpQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) processNextDeleteNpWorkItem() bool {
	obj, shutdown := c.deleteNpQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.deleteNpQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.deleteNpQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleDeleteNp(key); err != nil {
			c.deleteNpQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.deleteNpQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) handleUpdateNp(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	np, err := c.npsLister.NetworkPolicies(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	defer func() {
		if err != nil {
			c.recorder.Eventf(np, corev1.EventTypeWarning, "CreateACLFailed", err.Error())
		}
	}()

	ingressPorts := []netv1.NetworkPolicyPort{}
	egressPorts := []netv1.NetworkPolicyPort{}
	for _, npr := range np.Spec.Ingress {
		ingressPorts = append(ingressPorts, npr.Ports...)
	}
	for _, npr := range np.Spec.Egress {
		egressPorts = append(egressPorts, npr.Ports...)
	}

	pgName := fmt.Sprintf("%s.%s", np.Name, np.Namespace)

	// TODO: ovn acl doesn't support address_set name with '-', now we replace '-' by '.'.
	// This may cause conflict if two np with name test-np and test.np. Maybe hash is a better solution,
	// but we do not want to lost the readability now.
	ingressAllowAsName := strings.Replace(fmt.Sprintf("%s.%s.ingress.allow", np.Name, np.Namespace), "-", ".", -1)
	ingressExceptAsName := strings.Replace(fmt.Sprintf("%s.%s.ingress.except", np.Name, np.Namespace), "-", ".", -1)
	egressAllowAsName := strings.Replace(fmt.Sprintf("%s.%s.egress.allow", np.Name, np.Namespace), "-", ".", -1)
	egressExceptAsName := strings.Replace(fmt.Sprintf("%s.%s.egress.except", np.Name, np.Namespace), "-", ".", -1)

	if err := c.ovnClient.CreatePortGroup(pgName); err != nil {
		klog.Errorf("failed to create port group for np %s, %v", key, err)
		return err
	}

	ports, err := c.fetchSelectedPorts(np.Namespace, &np.Spec.PodSelector)
	if err != nil {
		klog.Errorf("failed to fetch ports, %v", err)
		return err
	}

	err = c.ovnClient.SetPortsToPortGroup(pgName, ports)
	if err != nil {
		klog.Errorf("failed to set port group, %v", err)
		return err
	}

	if hasIngressRule(np) {
		if err := c.ovnClient.CreateAddressSet(ingressAllowAsName); err != nil {
			klog.Errorf("failed to create address_set %s, %v", ingressAllowAsName, err)
			return err
		}

		if err := c.ovnClient.CreateAddressSet(ingressExceptAsName); err != nil {
			klog.Errorf("failed to create address_set %s, %v", ingressExceptAsName, err)
			return err
		}

		allows := []string{}
		excepts := []string{}
		for _, npr := range np.Spec.Ingress {
			for _, npp := range npr.From {
				allow, except, err := c.fetchPolicySelectedAddresses(np.Namespace, npp)
				if err != nil {
					klog.Errorf("failed to fetch policy selected addresses, %v", err)
					return err
				}
				allows = append(allows, allow...)
				excepts = append(excepts, except...)
			}
		}

		err = c.ovnClient.SetAddressesToAddressSet(allows, ingressAllowAsName)
		if err != nil {
			klog.Errorf("failed to set ingress allow address_set, %v", err)
			return err
		}

		err = c.ovnClient.SetAddressesToAddressSet(excepts, ingressExceptAsName)
		if err != nil {
			klog.Errorf("failed to set ingress except address_set, %v", err)
			return err
		}

		if err := c.ovnClient.CreateIngressACL(pgName, ingressAllowAsName, ingressExceptAsName, ingressPorts); err != nil {
			klog.Errorf("failed to create ingress acls for np %s, %v", key, err)
			return err
		}
	} else {
		if err := c.ovnClient.DeleteACL(pgName, "to-lport"); err != nil {
			klog.Errorf("failed to delete np %s ingress acls, %v", key, err)
			return err
		}

		if err := c.ovnClient.DeleteAddressSet(ingressAllowAsName); err != nil {
			klog.Errorf("failed to delete np %s ingress allow address set, %v", key, err)
			return err
		}

		if err := c.ovnClient.DeleteAddressSet(ingressExceptAsName); err != nil {
			klog.Errorf("failed to delete np %s ingress except address set, %v", key, err)
			return err
		}
	}

	if hasEgressRule(np) {
		if err := c.ovnClient.CreateAddressSet(egressAllowAsName); err != nil {
			klog.Errorf("failed to create address_set %s, %v", egressAllowAsName, err)
			return err
		}

		if err := c.ovnClient.CreateAddressSet(egressExceptAsName); err != nil {
			klog.Errorf("failed to create address_set %s, %v", egressExceptAsName, err)
			return err
		}

		allows := []string{}
		excepts := []string{}
		for _, npr := range np.Spec.Egress {
			for _, npp := range npr.To {
				allow, except, err := c.fetchPolicySelectedAddresses(np.Namespace, npp)
				if err != nil {
					klog.Errorf("failed to fetch policy selected addresses, %v", err)
					return err
				}
				allows = append(allows, allow...)
				excepts = append(excepts, except...)
			}
		}

		err = c.ovnClient.SetAddressesToAddressSet(allows, egressAllowAsName)
		if err != nil {
			klog.Errorf("failed to set egress allow address_set, %v", err)
			return err
		}

		err = c.ovnClient.SetAddressesToAddressSet(excepts, egressExceptAsName)
		if err != nil {
			klog.Errorf("failed to set egress except address_set, %v", err)
			return err
		}

		if err := c.ovnClient.CreateEgressACL(pgName, egressAllowAsName, egressExceptAsName, egressPorts); err != nil {
			klog.Errorf("failed to create egress acls for np %s, %v", key, err)
			return err
		}
	} else {
		if err := c.ovnClient.DeleteACL(pgName, "from-lport"); err != nil {
			klog.Errorf("failed to delete np %s egress acls, %v", key, err)
			return err
		}

		if err := c.ovnClient.DeleteAddressSet(egressAllowAsName); err != nil {
			klog.Errorf("failed to delete np %s egress allow address set, %v", key, err)
			return err
		}

		if err := c.ovnClient.DeleteAddressSet(egressExceptAsName); err != nil {
			klog.Errorf("failed to delete np %s egress except address set, %v", key, err)
			return err
		}
	}
	return nil
}

func (c *Controller) handleDeleteNp(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	pgName := fmt.Sprintf("%s.%s", name, namespace)
	ingressAllowAsName := strings.Replace(fmt.Sprintf("%s.%s.ingress.allow", name, namespace), "-", ".", -1)
	ingressExceptAsName := strings.Replace(fmt.Sprintf("%s.%s.ingress.except", name, namespace), "-", ".", -1)
	egressAllowAsName := strings.Replace(fmt.Sprintf("%s.%s.egress.allow", name, namespace), "-", ".", -1)
	egressExceptAsName := strings.Replace(fmt.Sprintf("%s.%s.egress.except", name, namespace), "-", ".", -1)

	if err := c.ovnClient.DeleteACL(pgName, "to-lport"); err != nil {
		klog.Errorf("failed to delete np %s ingress acls, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeleteACL(pgName, "from-lport"); err != nil {
		klog.Errorf("failed to delete np %s egress acls, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeleteAddressSet(ingressAllowAsName); err != nil {
		klog.Errorf("failed to delete np %s ingress allow address set, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeleteAddressSet(ingressExceptAsName); err != nil {
		klog.Errorf("failed to delete np %s ingress except address set, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeleteAddressSet(egressAllowAsName); err != nil {
		klog.Errorf("failed to delete np %s egress allow address set, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeleteAddressSet(egressExceptAsName); err != nil {
		klog.Errorf("failed to delete np %s egress except address set, %v", key, err)
		return err
	}

	if err := c.ovnClient.DeletePortGroup(pgName); err != nil {
		klog.Errorf("failed to delete np %s port group, %v", key, err)
	}

	return nil
}

func (c *Controller) fetchSelectedPorts(namespace string, selector *metav1.LabelSelector) ([]string, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("error createing label selector, %v", err)
	}
	pods, err := c.podsLister.Pods(namespace).List(sel)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods, %v", err)
	}

	ports := make([]string, 0, len(pods))
	for _, pod := range pods {
		if !pod.Spec.HostNetwork {
			ports = append(ports, fmt.Sprintf("%s.%s", pod.Name, pod.Namespace))
		}
	}
	return ports, nil
}

func hasIngressRule(np *netv1.NetworkPolicy) bool {
	for _, pt := range np.Spec.PolicyTypes {
		if strings.Contains(string(pt), string(netv1.PolicyTypeIngress)) {
			return true
		}
	}
	if np.Spec.Ingress != nil {
		return true
	}
	return false
}

func hasEgressRule(np *netv1.NetworkPolicy) bool {
	for _, pt := range np.Spec.PolicyTypes {
		if strings.Contains(string(pt), string(netv1.PolicyTypeEgress)) {
			return true
		}
	}
	if np.Spec.Egress != nil {
		return true
	}
	return false
}

func (c *Controller) fetchPolicySelectedAddresses(namespace string, npp netv1.NetworkPolicyPeer) ([]string, []string, error) {
	if npp.IPBlock != nil {
		return []string{npp.IPBlock.CIDR}, npp.IPBlock.Except, nil
	}

	selectedNs := []string{}
	if npp.NamespaceSelector == nil {
		selectedNs = append(selectedNs, namespace)
	} else {
		sel, err := metav1.LabelSelectorAsSelector(npp.NamespaceSelector)
		if err != nil {
			return nil, nil, fmt.Errorf("error createing label selector, %v", err)
		}
		nss, err := c.namespacesLister.List(sel)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to list ns, %v", err)
		}
		for _, ns := range nss {
			selectedNs = append(selectedNs, ns.Name)
		}
	}

	selectedAddresses := []string{}
	var sel labels.Selector
	if npp.PodSelector == nil {
		sel = labels.Everything()
	} else {
		sel, _ = metav1.LabelSelectorAsSelector(npp.PodSelector)
	}

	for _, ns := range selectedNs {
		pods, err := c.podsLister.Pods(ns).List(sel)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to list pod, %v", err)
		}
		for _, pod := range pods {
			if pod.Annotations[util.IpAddressAnnotation] != "" {
				selectedAddresses = append(selectedAddresses, strings.Split(pod.Annotations[util.IpAddressAnnotation], "/")[0])
			} else {
				if pod.Status.PodIP != "" {
					selectedAddresses = append(selectedAddresses, pod.Status.PodIP)
				}
			}
		}
	}
	return selectedAddresses, nil, nil
}

func (c *Controller) podMatchNetworkPolicies(pod *corev1.Pod) []string {
	podNs, _ := c.namespacesLister.Get(pod.Namespace)
	nps, _ := c.npsLister.NetworkPolicies(corev1.NamespaceAll).List(labels.Everything())
	match := []string{}
	for _, np := range nps {
		if isPodMatchNetworkPolicy(pod, podNs, np, np.Namespace) {
			match = append(match, fmt.Sprintf("%s/%s", np.Namespace, np.Name))
		}
	}
	return match
}

func isPodMatchNetworkPolicy(pod *corev1.Pod, podNs *corev1.Namespace, policy *netv1.NetworkPolicy, policyNs string) bool {
	sel, _ := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if sel.Matches(labels.Set(pod.Labels)) {
		return true
	}
	for _, npr := range policy.Spec.Ingress {
		for _, npp := range npr.From {
			if isPodMatchPolicyPeer(pod, podNs, &npp, policyNs) {
				return true
			}
		}
	}
	for _, npr := range policy.Spec.Egress {
		for _, npp := range npr.To {
			if isPodMatchPolicyPeer(pod, podNs, &npp, policyNs) {
				return true
			}
		}
	}
	return false
}

func isPodMatchPolicyPeer(pod *corev1.Pod, podNs *corev1.Namespace, policyPeer *netv1.NetworkPolicyPeer, policyNs string) bool {
	if policyPeer.IPBlock != nil {
		return false
	}
	if policyPeer.NamespaceSelector == nil {
		if policyNs != podNs.Name {
			return false
		}

	} else {
		nsSel, _ := metav1.LabelSelectorAsSelector(policyPeer.NamespaceSelector)
		if !nsSel.Matches(labels.Set(podNs.Labels)) {
			return false
		}
	}

	if policyPeer.PodSelector == nil {
		return true
	}

	sel, _ := metav1.LabelSelectorAsSelector(policyPeer.PodSelector)
	return sel.Matches(labels.Set(pod.Labels))
}

func (c *Controller) namespaceMatchNetworkPolicies(ns *corev1.Namespace) []string {
	nps, _ := c.npsLister.NetworkPolicies(corev1.NamespaceAll).List(labels.Everything())
	match := []string{}
	for _, np := range nps {
		if isNamespaceMatchNetworkPolicy(ns, np) {
			match = append(match, fmt.Sprintf("%s/%s", np.Namespace, np.Name))
		}
	}
	return match
}

func isNamespaceMatchNetworkPolicy(ns *corev1.Namespace, policy *netv1.NetworkPolicy) bool {
	for _, npr := range policy.Spec.Ingress {
		for _, npp := range npr.From {
			if npp.NamespaceSelector != nil {
				nsSel, _ := metav1.LabelSelectorAsSelector(npp.NamespaceSelector)
				if nsSel.Matches(labels.Set(ns.Labels)) {
					return true
				}
			}
		}
	}

	for _, npr := range policy.Spec.Egress {
		for _, npp := range npr.To {
			if npp.NamespaceSelector != nil {
				nsSel, _ := metav1.LabelSelectorAsSelector(npp.NamespaceSelector)
				if nsSel.Matches(labels.Set(ns.Labels)) {
					return true
				}
			}
		}
	}
	return false
}
