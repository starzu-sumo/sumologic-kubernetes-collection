package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal"
	"github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal/ctxopts"
	"github.com/SumoLogic/sumologic-kubernetes-collection/tests/integration/internal/stepfuncs"
)

func Test_Non_Helm_Default(t *testing.T) {
	var (
		now         = time.Now()
		namespace   = generateNamespaceName(now)
		releaseName = generateReleaseName(now)

		valuesFilePath         = "values/values_default.yaml"
		defaultK8sApiVersions  = []string{"policy/v1/PodDisruptionBudget"}
		prometheusOperatorCrds = []string{
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_probes.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_alertmanagers.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_prometheuses.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_prometheusrules.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml",
			"https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.43.2/example/prometheus-operator-crd/monitoring.coreos.com_alertmanagerconfigs.yaml",
		}

		tickDuration = time.Second
		waitDuration = time.Minute
	)

	templatedFile, err := ioutil.TempFile(os.TempDir(), releaseName)
	if err != nil {
		t.Fatal(err)
	}
	templatedFilePath := templatedFile.Name()
	defer os.Remove(templatedFilePath)

	feat := features.New("installation").
		// Setup
		Setup(stepfuncs.SetKubectlNamespaceOpt(namespace)).
		Setup(stepfuncs.KubectlApplyFOpt(internal.YamlPathReceiverMock, "receiver-mock")).
		Setup(stepfuncs.SetHelmOptionsOpt(valuesFilePath)).
		Setup(stepfuncs.HelmDependencyUpdateOpt(internal.HelmSumoLogicChartAbsPath)).
		Setup(
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				kubectlOpts := *ctxopts.KubectlOptions(ctx)
				prometheusCrdArgs := []string{"apply"}
				for _, v := range prometheusOperatorCrds {
					prometheusCrdArgs = append(prometheusCrdArgs, "-f", v)
				}
				k8s.RunKubectl(t, &kubectlOpts, prometheusCrdArgs...)
				return ctx
			}).
		Setup(
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				kubectlOpts := *ctxopts.KubectlOptions(ctx)
				k8s.RunKubectl(t, &kubectlOpts, "config", "set-context", "--current", "--namespace="+namespace)
				return ctx
			}).
		// Use helm template instead of the tools image, as it's much easier to run using Terratest's utilities
		// and roughly equivalent in function.
		// TODO: Use kubernetes-tools instead as the docs recommend
		Setup(stepfuncs.HelmTemplateOpt(internal.HelmSumoLogicChartAbsPath, releaseName, templatedFilePath, valuesFilePath, defaultK8sApiVersions[:])).
		Setup(stepfuncs.KubectlApplyFOpt(templatedFilePath, "")).
		// Teardown
		Teardown(stepfuncs.PrintClusterStateOpt()).
		Teardown( // we want to ignore errors here
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				kubectlOpts := *ctxopts.KubectlOptions(ctx)
				k8s.KubectlDeleteE(t, &kubectlOpts, templatedFilePath)
				return ctx
			}).
		Teardown(stepfuncs.KubectlDeleteNamespaceOpt(namespace)).
		Teardown(stepfuncs.KubectlDeleteFOpt(internal.YamlPathReceiverMock, "receiver-mock")).
		// Assess
		// Assess
		Assess("sumologic secret is created",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				k8s.WaitUntilSecretAvailable(t, ctxopts.KubectlOptions(ctx), "sumologic", 120, tickDuration)
				secret := k8s.GetSecret(t, ctxopts.KubectlOptions(ctx), "sumologic")
				require.Len(t, secret.Data, 10)
				return ctx
			}).
		Assess("3 fluentd logs pods are created and running",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				require.Eventually(t, func() bool {
					pods := k8s.ListPods(t, ctxopts.KubectlOptions(ctx), v1.ListOptions{
						LabelSelector: fmt.Sprintf("app=%s-sumologic-fluentd-logs", releaseName),
						FieldSelector: "status.phase=Running",
					})
					return len(pods) == 3
				}, waitDuration, tickDuration)

				return ctx
			}).
		Assess("3 fluentd logs buffers PVCs are created",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				assert.Eventually(t, func() bool {
					var pvcs corev1.PersistentVolumeClaimList
					err := envConf.Client().
						Resources(namespace).
						List(ctx, &pvcs,
							resources.WithLabelSelector(fmt.Sprintf("app=%s-sumologic-fluentd-logs", releaseName)),
						)
					return err == nil && len(pvcs.Items) == 3
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("3 fluentd metrics pods are created and running",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				require.Eventually(t, func() bool {
					pods := k8s.ListPods(t, ctxopts.KubectlOptions(ctx), v1.ListOptions{
						LabelSelector: fmt.Sprintf("app=%s-sumologic-fluentd-metrics", releaseName),
						FieldSelector: "status.phase=Running",
					})
					return len(pods) == 3
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("3 fluentd metrics buffers PVCs are created",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				assert.Eventually(t, func() bool {
					var pvcs corev1.PersistentVolumeClaimList
					err := envConf.Client().
						Resources(namespace).
						List(ctx, &pvcs,
							resources.WithLabelSelector(fmt.Sprintf("app=%s-sumologic-fluentd-metrics", releaseName)),
						)
					return err == nil && len(pvcs.Items) == 3
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("1 fluentd events pod is created and running",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				require.Eventually(t, func() bool {
					pods := k8s.ListPods(t, ctxopts.KubectlOptions(ctx), v1.ListOptions{
						LabelSelector: fmt.Sprintf("app=%s-sumologic-fluentd-events", releaseName),
						FieldSelector: "status.phase=Running",
					})
					return len(pods) == 1
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("1 fluentd events buffers PVCs are created",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				assert.Eventually(t, func() bool {
					var pvcs corev1.PersistentVolumeClaimList
					err := envConf.Client().
						Resources(namespace).
						List(ctx, &pvcs,
							resources.WithLabelSelector(fmt.Sprintf("app=%s-sumologic-fluentd-events", releaseName)),
						)
					return err == nil && len(pvcs.Items) == 1
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("1 prometheus pod is created and running",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				require.Eventually(t, func() bool {
					pods := k8s.ListPods(t, ctxopts.KubectlOptions(ctx), v1.ListOptions{
						LabelSelector: "app=prometheus",
						FieldSelector: "status.phase=Running",
					})
					return len(pods) == 1
				}, waitDuration, tickDuration)
				return ctx
			}).
		Assess("fluent-bit daemonset is running",
			func(ctx context.Context, t *testing.T, envConf *envconf.Config) context.Context {
				var daemonsets []appsv1.DaemonSet
				require.Eventually(t, func() bool {
					daemonsets = k8s.ListDaemonSets(t, ctxopts.KubectlOptions(ctx), v1.ListOptions{
						LabelSelector: "app.kubernetes.io/name=fluent-bit",
					})

					return len(daemonsets) == 1
				}, waitDuration, tickDuration)

				require.EqualValues(t, 0, daemonsets[0].Status.NumberUnavailable)
				return ctx
			}).
		Feature()

	testenv.Test(t, feat)
}
