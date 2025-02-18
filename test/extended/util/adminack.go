package util

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

// AdminAckTest contains artifacts used during test
type AdminAckTest struct {
	Oc     *CLI
	Config *restclient.Config
}

const adminAckGateFmt string = "^ack-[4-5][.]([0-9]{1,})-[^-]"

var adminAckGateRegexp = regexp.MustCompile(adminAckGateFmt)

// Test simply returns successfully if admin ack functionality is not part of the baseline being tested. Otherwise,
// for each configured admin ack gate, test verifies the gate name format and that it contains a description. If
// valid and the gate is applicable to the OCP version under test, test checks the value of the admin ack gate.
// If the gate has been ack'ed the test verifies that Upgradeable condition is true and clears the ack. Test then
// verifies Upgradeable condition is false and contains correct reason and correct message. It then modifies the
// admin-acks configmap to ack the given admin-ack gate. Once all gates have been ack'ed, the test waits for the
// Upgradeable condition to change to true.
func (t *AdminAckTest) Test(ctx context.Context) {

	gateCm, errMsg := getAdminGatesConfigMap(ctx, t.Oc)
	if len(errMsg) != 0 {
		framework.Failf(errMsg)
	}
	// Check if this release has admin ack functionality.
	if gateCm == nil || (gateCm != nil && len(gateCm.Data) == 0) {
		framework.Logf("Skipping admin ack test. Admin ack is not in this baseline or contains no gates.")
		return
	}
	ackCm, errMsg := getAdminAcksConfigMap(ctx, t.Oc)
	if len(errMsg) != 0 {
		framework.Failf(errMsg)
	}
	currentVersion := getCurrentVersion(ctx, t.Config)
	var msg string
	for k, v := range gateCm.Data {
		ackVersion := adminAckGateRegexp.FindString(k)
		if ackVersion == "" {
			framework.Failf(fmt.Sprintf("Configmap openshift-config-managed/admin-gates gate %s has invalid format; must comply with %q.", k, adminAckGateFmt))
		}
		if v == "" {
			framework.Failf(fmt.Sprintf("Configmap openshift-config-managed/admin-gates gate %s does not contain description.", k))
		}
		if !gateApplicableToCurrentVersion(ackVersion, currentVersion) {
			continue
		}
		if ackCm.Data[k] == "true" {
			if upgradeableExplicitlyFalse(ctx, t.Config) {
				if adminAckRequiredWithMessage(ctx, t.Config, v) {
					framework.Failf(fmt.Sprintf("Gate %s has been ack'ed but Upgradeable is "+
						"false with reason AdminAckRequired and message %q.", k, v))
				}
				framework.Logf(fmt.Sprintf("Gate %s has been ack'ed. Upgradeable is "+
					"false but not due to this gate which would set reason AdminAckRequired with message %s.", k, v) +
					" " + getUpgradeable(ctx, t.Config))
			}
			// Clear admin ack configmap gate ack
			if errMsg = setAdminGate(ctx, k, "", t.Oc); len(errMsg) != 0 {
				framework.Failf(errMsg)
			}
		}
		if errMsg = waitForAdminAckRequired(ctx, t.Config, msg); len(errMsg) != 0 {
			framework.Failf(errMsg)
		}
		// Update admin ack configmap with ack
		if errMsg = setAdminGate(ctx, k, "true", t.Oc); len(errMsg) != 0 {
			framework.Failf(errMsg)
		}
	}
	if errMsg = waitForUpgradeable(ctx, t.Config); len(errMsg) != 0 {
		framework.Failf(errMsg)
	}
	framework.Logf("Admin Ack verified")
}

// getClusterVersion returns the ClusterVersion object.
func getClusterVersion(ctx context.Context, config *restclient.Config) *configv1.ClusterVersion {
	c, err := configv1client.NewForConfig(config)
	if err != nil {
		framework.Failf(fmt.Sprintf("Error getting config, err=%v", err))
	}
	cv, err := c.ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		framework.Failf(fmt.Sprintf("Error getting custer version, err=%v", err))
	}
	return cv
}

// getCurrentVersion determines and returns the cluster's current version by iterating through the
// provided update history until it finds the first version with update State of Completed. If a
// Completed version is not found the version of the oldest history entry, which is the originally
// installed version, is returned. If history is empty the empty string is returned.
func getCurrentVersion(ctx context.Context, config *restclient.Config) string {
	cv := getClusterVersion(ctx, config)
	for _, h := range cv.Status.History {
		if h.State == configv1.CompletedUpdate {
			return h.Version
		}
	}
	// Empty history should only occur if method is called early in startup before history is populated.
	if len(cv.Status.History) != 0 {
		return cv.Status.History[len(cv.Status.History)-1].Version
	}
	return ""
}

// getEffectiveMinor attempts to do a simple parse of the version provided.  If it does not parse, the value is considered
// an empty string, which works for a comparison for equivalence.
func getEffectiveMinor(version string) string {
	splits := strings.Split(version, ".")
	if len(splits) < 2 {
		return ""
	}
	return splits[1]
}

func gateApplicableToCurrentVersion(gateAckVersion string, currentVersion string) bool {
	parts := strings.Split(gateAckVersion, "-")
	ackMinor := getEffectiveMinor(parts[1])
	cvMinor := getEffectiveMinor(currentVersion)
	if ackMinor == cvMinor {
		return true
	}
	return false
}

func getAdminGatesConfigMap(ctx context.Context, oc *CLI) (*corev1.ConfigMap, string) {
	cm, err := oc.AdminKubeClient().CoreV1().ConfigMaps("openshift-config-managed").Get(ctx, "admin-gates", metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("Error accessing configmap openshift-config-managed/admin-gates, err=%v", err)
		} else {
			return nil, ""
		}
	}
	return cm, ""
}

func getAdminAcksConfigMap(ctx context.Context, oc *CLI) (*corev1.ConfigMap, string) {
	cm, err := oc.AdminKubeClient().CoreV1().ConfigMaps("openshift-config").Get(ctx, "admin-acks", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Sprintf("Error accessing configmap openshift-config/admin-acks, err=%v", err)
	}
	return cm, ""
}

// adminAckRequiredWithMessage returns true if Upgradeable condition reason contains AdminAckRequired
// and message contains given message.
func adminAckRequiredWithMessage(ctx context.Context, config *restclient.Config, message string) bool {
	clusterVersion := getClusterVersion(ctx, config)
	cond := getUpgradeableStatusCondition(clusterVersion.Status.Conditions)
	if cond != nil && strings.Contains(cond.Reason, "AdminAckRequired") && strings.Contains(cond.Message, message) {
		return true
	}
	return false
}

// upgradeableExplicitlyFalse returns true if the Upgradeable condition status is set to false.
func upgradeableExplicitlyFalse(ctx context.Context, config *restclient.Config) bool {
	clusterVersion := getClusterVersion(ctx, config)
	cond := getUpgradeableStatusCondition(clusterVersion.Status.Conditions)
	if cond != nil && cond.Status == configv1.ConditionFalse {
		return true
	}
	return false
}

// setAdminGate gets the admin ack configmap and then updates it with given gate name and given value.
func setAdminGate(ctx context.Context, gateName string, gateValue string, oc *CLI) string {
	ackCm, errMsg := getAdminAcksConfigMap(ctx, oc)
	if len(errMsg) != 0 {
		framework.Failf(errMsg)
	}
	ackCm.Data[gateName] = gateValue
	_, err := oc.AdminKubeClient().CoreV1().ConfigMaps("openshift-config").Update(ctx, ackCm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Sprintf("Unable to update configmap openshift-config/admin-acks, err=%v.", err)
	}
	return ""
}

func waitForAdminAckRequired(ctx context.Context, config *restclient.Config, message string) string {
	framework.Logf("Waiting for Upgradeable to be AdminAckRequired...")
	if err := wait.PollImmediate(10*time.Second, 3*time.Minute, func() (bool, error) {
		if adminAckRequiredWithMessage(ctx, config, message) {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return fmt.Sprintf("Error while waiting for Upgradeable to go AdminAckRequired with message %q, err=%v", message, err) +
			" " + getUpgradeable(ctx, config)
	}
	return ""
}

func waitForUpgradeable(ctx context.Context, config *restclient.Config) string {
	framework.Logf("Waiting for Upgradeable true...")
	if err := wait.PollImmediate(10*time.Second, 3*time.Minute, func() (bool, error) {
		if !upgradeableExplicitlyFalse(ctx, config) {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return fmt.Sprintf("Error while waiting for Upgradeable to go true, err=%v", err) + " " + getUpgradeable(ctx, config)
	}
	return ""
}

func getUpgradeableStatusCondition(conditions []configv1.ClusterOperatorStatusCondition) *configv1.ClusterOperatorStatusCondition {
	for _, condition := range conditions {
		if condition.Type == configv1.OperatorUpgradeable {
			return &condition
		}
	}
	return nil
}

func getUpgradeable(ctx context.Context, config *restclient.Config) string {
	clusterVersion := getClusterVersion(ctx, config)
	cond := getUpgradeableStatusCondition(clusterVersion.Status.Conditions)
	if cond != nil {
		return fmt.Sprintf("Upgradeable: Status=%s, Reason=%s, Message=%q.", cond.Status, cond.Reason, cond.Message)
	}
	return "Upgradeable nil"
}
