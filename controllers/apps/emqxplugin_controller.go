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

package apps

import (
	"context"
	"fmt"
	"strings"
	"time"

	json "github.com/json-iterator/go"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1beta3 "github.com/emqx/emqx-operator/apis/apps/v1beta3"
)

// EmqxPluginReconciler reconciles a EmqxPlugin object
type EmqxPluginReconciler struct {
	Handler
}

//+kubebuilder:rbac:groups=apps.emqx.io,resources=emqxplugins,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.emqx.io,resources=emqxplugins/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.emqx.io,resources=emqxplugins/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the EmqxPlugin object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *EmqxPluginReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.V(1).Info("Reconcile EmqxPlugin")

	instance := &appsv1beta3.EmqxPlugin{}
	if err := r.Handler.Get(ctx, req.NamespacedName, instance); err != nil {
		if k8sErrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	instance.APIVersion = appsv1beta3.GroupVersion.Group + "/" + appsv1beta3.GroupVersion.Version
	instance.Kind = "EmqxPlugin"

	emqxList, err := r.getEmqxList(instance.Namespace, instance.Spec.Selector)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			logger.V(1).Info("Not matched emqx")
			return ctrl.Result{RequeueAfter: time.Duration(30) * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	finalizer := "apps.emqx.io/finalizer"
	if instance.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(instance, finalizer) {
			for _, emqx := range emqxList {
				if err := r.unloadPluginToEmqx(instance, emqx); err != nil {
					return ctrl.Result{}, err
				}
			}

			// Remove Finalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(instance, finalizer)
			err := r.Update(ctx, instance)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(instance, finalizer) {
		controllerutil.AddFinalizer(instance, finalizer)
		err := r.Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, emqx := range emqxList {
		if err := r.configurePluginToEmqx(instance, emqx); err != nil {
			return ctrl.Result{}, err
		}
	}
	instance.Status.Phase = appsv1beta3.EmqxPluginStatusConfigured
	_ = r.Status().Update(ctx, instance)

	for _, emqx := range emqxList {
		if err := r.loadPluginToEmqx(instance, emqx); err != nil {
			if err.Error() == "need requeue" {
				logger.V(1).Info("Load plugin to emqx failed, need requeue")
				return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	instance.Status.Phase = appsv1beta3.EmqxPluginStatusLoaded
	_ = r.Status().Update(ctx, instance)
	return ctrl.Result{RequeueAfter: time.Duration(30) * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EmqxPluginReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1beta3.EmqxPlugin{}).
		Complete(r)
}

func generateConfigStr(plugin *appsv1beta3.EmqxPlugin) string {
	var config string
	for key, value := range plugin.Spec.Config {
		config += fmt.Sprintln(key, " = ", value)
	}
	return config
}
func (r *EmqxPluginReconciler) configurePluginToEmqx(plugin *appsv1beta3.EmqxPlugin, emqx appsv1beta3.Emqx) error {
	pluginConfigStr := generateConfigStr(plugin)

	pluginsConfig, err := r.getPluginsConfig(emqx)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	configMapStr, err := json.ConfigCompatibleWithStandardLibrary.Marshal(pluginsConfig.Data)
	if err != nil {
		return err
	}

	path := plugin.Spec.PluginName + "\\.conf"
	storePluginConfig := gjson.GetBytes(configMapStr, path)
	if storePluginConfig.Exists() {
		if storePluginConfig.String() == pluginConfigStr {
			return nil
		}
	}
	newConfigMapStr, err := sjson.SetBytes(configMapStr, path, pluginConfigStr)
	if err != nil {
		return err
	}

	configData := map[string]string{}
	if err := json.Unmarshal(newConfigMapStr, &configData); err != nil {
		return err
	}
	pluginsConfig.Data = configData

	// Update plugin config
	if err := r.doUpdate(pluginsConfig, func(_ client.Object) error { return nil }); err != nil {
		return err
	}
	return nil
}

func (r *EmqxPluginReconciler) loadPluginToEmqx(plugin *appsv1beta3.EmqxPlugin, emqx appsv1beta3.Emqx) error {
	// Reload plugin
	err := r.ExecToPods(emqx, "emqx", "emqx_ctl plugins reload "+plugin.Spec.PluginName)
	if err != nil {
		return fmt.Errorf("need requeue")
	}

	// Update loaded plugins
	loadedPlugins, err := r.getLoadedPlugins(emqx)
	if err != nil {
		return err
	}
	loadedPluginsStr := loadedPlugins.Data["loaded_plugins"]
	loadedPluginLine := fmt.Sprintf("{%s, true}.\n", plugin.Spec.PluginName)
	index := strings.Index(loadedPluginsStr, loadedPluginLine)
	if index == -1 {
		loadedPluginsStr += loadedPluginLine
		loadedPlugins.Data = map[string]string{"loaded_plugins": loadedPluginsStr}
		if err := r.doUpdate(loadedPlugins, func(_ client.Object) error { return nil }); err != nil {
			return err
		}
	}

	return nil
}

func (r *EmqxPluginReconciler) unloadPluginToEmqx(plugin *appsv1beta3.EmqxPlugin, emqx appsv1beta3.Emqx) error {

	pluginsConfig, err := r.getPluginsConfig(emqx)
	if err != nil {
		return err
	}

	configMapStr, err := json.ConfigCompatibleWithStandardLibrary.Marshal(pluginsConfig.Data)
	if err != nil {
		return err
	}

	path := plugin.Spec.PluginName + "\\.conf"
	storePluginConfig := gjson.GetBytes(configMapStr, path)
	if !storePluginConfig.Exists() {
		return nil
	}

	// Unload plugin
	_ = r.ExecToPods(emqx, "emqx", "emqx_ctl plugins unload "+plugin.Spec.PluginName)

	// Update plugin config
	newConfigMapStr, err := sjson.DeleteBytes(configMapStr, path)
	if err != nil {
		return err
	}

	configData := map[string]string{}
	if err := json.Unmarshal(newConfigMapStr, &configData); err != nil {
		return err
	}
	pluginsConfig.Data = configData

	postfun := func(_ client.Object) error { return nil }
	if err := r.doUpdate(pluginsConfig, postfun); err != nil {
		return err
	}

	// Update loaded plugins
	loadedPlugins, err := r.getLoadedPlugins(emqx)
	if err != nil {
		return err
	}
	loadedPluginsStr := loadedPlugins.Data["loaded_plugins"]
	loadedPluginLine := fmt.Sprintf("{%s, true}.\n", plugin.Spec.PluginName)
	index := strings.Index(loadedPluginsStr, loadedPluginLine)
	if index != -1 {
		loadedPluginsStr = loadedPluginsStr[:index] + loadedPluginsStr[index+len(loadedPluginLine):]
		loadedPlugins.Data = map[string]string{"loaded_plugins": loadedPluginsStr}
		if err := r.doUpdate(loadedPlugins, postfun); err != nil {
			return err
		}
	}
	return nil
}

func (r *EmqxPluginReconciler) getEmqxList(namespace string, labels map[string]string) ([]appsv1beta3.Emqx, error) {
	var emqxList []appsv1beta3.Emqx

	emqxBrokerList := &appsv1beta3.EmqxBrokerList{}
	if err := r.List(context.Background(), emqxBrokerList, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		if !k8sErrors.IsNotFound(err) {
			return nil, err
		}
	}
	for _, emqxBroker := range emqxBrokerList.Items {
		emqxList = append(emqxList, &emqxBroker)
	}

	emqxEnterpriseList := &appsv1beta3.EmqxEnterpriseList{}
	if err := r.List(context.Background(), emqxEnterpriseList, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		if !k8sErrors.IsNotFound(err) {
			return nil, err
		}
	}
	for _, emqxEnterprise := range emqxEnterpriseList.Items {
		emqxList = append(emqxList, &emqxEnterprise)
	}

	return emqxList, nil
}

func (r *EmqxPluginReconciler) getPluginsConfig(emqx appsv1beta3.Emqx) (*corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{}
	if err := r.Get(
		context.TODO(),
		client.ObjectKey{
			Name:      fmt.Sprintf("%s-%s", emqx.GetName(), "plugins-config"),
			Namespace: emqx.GetNamespace(),
		},
		configMap,
	); err != nil {
		return nil, err
	}
	return configMap, nil
}

func (r *EmqxPluginReconciler) getLoadedPlugins(emqx appsv1beta3.Emqx) (*corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{}
	if err := r.Get(
		context.TODO(),
		client.ObjectKey{
			Name:      fmt.Sprintf("%s-%s", emqx.GetName(), "loaded-plugins"),
			Namespace: emqx.GetNamespace(),
		},
		configMap,
	); err != nil {
		return nil, err
	}
	return configMap, nil
}