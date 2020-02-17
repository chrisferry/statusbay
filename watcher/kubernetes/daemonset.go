//reference: https://gitlab.similarweb.io/elad.kaplan/statusier-open-source/blob/test_replicaset/watcher/kubernetes/daemonset.go
package kuberneteswatcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mitchellh/hashstructure"
	log "github.com/sirupsen/logrus"
	appsV1 "k8s.io/api/apps/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	eventwatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type DaemonsetManager struct {
	// Kubernetes client
	client kubernetes.Interface

	// Event manager will be owner to start watch on deployment events
	eventManager *EventsManager

	// Registry manager will be owner to manage the running / new deployment
	registryManager *RegistryManager

	// Will triggered when deployment watch started
	serviceManager *ServiceManager

	//
	controllerRevManager ControllerRevision
	// Max watch time
	maxDeploymentTime int64
}

//NewDaemonsetManager  create new instance to manage damonset related things
func NewDaemonsetManager(kubernetesClientset kubernetes.Interface, eventManager *EventsManager, registryManager *RegistryManager, serviceManager *ServiceManager, controllerRevisionManager ControllerRevision, maxDeploymentTime time.Duration) *DaemonsetManager {
	return &DaemonsetManager{
		client:               kubernetesClientset,
		eventManager:         eventManager,
		registryManager:      registryManager,
		serviceManager:       serviceManager,
		controllerRevManager: controllerRevisionManager,
		maxDeploymentTime:    int64(maxDeploymentTime.Seconds()),
	}
}

func (dsm *DaemonsetManager) Serve(ctx context.Context, wg *sync.WaitGroup) {

	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Warn("Daemonset Manager has been shut down")
				wg.Done()
				return
			}
		}
	}()
	// continue running daemonsets from storage state
	runningDaemonsetsApps := dsm.registryManager.LoadRunningApplies()
	for _, application := range runningDaemonsetsApps {
		for _, daemonsetData := range application.DBSchema.Resources.Daemonsets {
			daemonsetWatchListOptions := metaV1.ListOptions{
				LabelSelector: labels.SelectorFromSet(daemonsetData.Metadata.Labels).String(),
			}
			go dsm.watchDaemonset(
				application.ctx,
				application.cancelFn,
				application.Log(),
				daemonsetData,
				daemonsetWatchListOptions,
				daemonsetData.Metadata.Namespace,
				daemonsetData.ProgressDeadlineSeconds,
			)
		}
	}
	dsm.watchDaemonsets(ctx)

}

func (dsm *DaemonsetManager) watchDaemonsets(ctx context.Context) {
	daemonsetsList, _ := dsm.client.AppsV1().DaemonSets("").List(metaV1.ListOptions{})
	daemonsetWatchListOptions := metaV1.ListOptions{ResourceVersion: daemonsetsList.GetResourceVersion()}
	watcher, err := dsm.client.AppsV1().DaemonSets("").Watch(daemonsetWatchListOptions)
	if err != nil {
		log.WithError(err).WithField("list_option", daemonsetWatchListOptions.String()).Error("Could not start a watcher on daemonsets")
		return
	}
	go func() {
		log.WithField("resource_version", daemonsetsList.GetResourceVersion()).Info("Daemonsets watcher was started")
		for {
			select {
			case event, watch := <-watcher.ResultChan():
				if !watch {
					log.WithField("list_options", daemonsetWatchListOptions.String()).Info("Daemonsets watcher was stopped. Reopen the channel")
					dsm.watchDaemonsets(ctx)
					return
				}
				daemonset, ok := event.Object.(*appsV1.DaemonSet)
				if !ok {
					log.WithField("object", event.Object).Warn("Failed to parse daemonset watcher data")
					continue
				}

				daemonsetName := GetApplicationName(daemonset.GetAnnotations(), daemonset.GetName())

				if event.Type == eventwatch.Modified || event.Type == eventwatch.Added || event.Type == eventwatch.Deleted {

					hash, _ := hashstructure.Hash(daemonset.Spec, nil)
					apply := ApplyEvent{
						Event:           fmt.Sprintf("%v", event.Type),
						ApplyName:       daemonsetName,
						ResourceName:    daemonset.GetName(),
						Namespace:       daemonset.GetNamespace(),
						Kind:            "daemonset",
						Hash:            hash,
						RegistryManager: dsm.registryManager,
						Annotations:     daemonset.GetAnnotations(),
					}

					appRegistry := dsm.registryManager.NewApplyEvent(apply)
					if appRegistry == nil {
						continue
					}

					registryApply := dsm.AddNewDaemonset(apply, appRegistry, daemonset.Status.DesiredNumberScheduled)

					daemonsetWatchListOptions := metaV1.ListOptions{
						LabelSelector: labels.SelectorFromSet(daemonset.GetLabels()).String()}

					go dsm.watchDaemonset(
						appRegistry.ctx,
						appRegistry.cancelFn,
						appRegistry.Log(),
						registryApply,
						daemonsetWatchListOptions,
						daemonset.GetNamespace(),
						GetProgressDeadlineApply(daemonset.GetAnnotations(), dsm.maxDeploymentTime))

				} else {
					log.WithFields(log.Fields{
						"event_type": event.Type,
						"deamonset":  daemonsetName,
					}).Info("Event type not supported")
				}
			case <-ctx.Done():
				log.Warn("Daemonset watch was stopped. Got ctx done signal")
				watcher.Stop()
				return
			}
		}
	}()
}

// watchDaemonset will watch a specific daemonset and its related resources (controller revision + pods)
func (dsm *DaemonsetManager) watchDaemonset(ctx context.Context, cancelFn context.CancelFunc, lg log.Entry, daemonsetData *DaemonsetData, listOptions metaV1.ListOptions, namespace string, maxWatchTime int64) {

	daemonsetLog := lg.WithField("daemonset_name", daemonsetData.GetName())
	daemonsetLog.Info("Starting watch on Daemonset")
	daemonsetLog.WithField("list_option", listOptions.String()).Debug("List option for daemonset filtering")

	watcher, err := dsm.client.AppsV1().DaemonSets(namespace).Watch(listOptions)
	if err != nil {
		daemonsetLog.WithError(err).Error("Could not start watch on daemonset")
		return
	}
	firstInit := true
	for {
		select {
		case event, watch := <-watcher.ResultChan():
			if !watch {
				daemonsetLog.Warn("Daemonset watcher was stopped. Channel was closed")
				cancelFn()
				return
			}
			daemonset, isOk := event.Object.(*appsV1.DaemonSet)
			if !isOk {
				daemonsetLog.WithField("object", event.Object).Warn("Failed to parse daemonset watcher data")
				continue
			}
			if firstInit {
				firstInit = false
				eventListOptions := metaV1.ListOptions{FieldSelector: labels.SelectorFromSet(map[string]string{
					"involvedObject.name": daemonset.GetName(),
					"involvedObject.kind": "DaemonSet",
				}).String(),
					TimeoutSeconds: &maxWatchTime,
				}
				dsm.watchEvents(ctx, *daemonsetLog, daemonsetData, eventListOptions, namespace)

				// start pods watch
				dsm.controllerRevManager.WatchControllerRevisionPodsRetry(ctx, *daemonsetLog, daemonsetData,
					daemonset.ObjectMeta.Generation,
					daemonset.Spec.Selector.MatchLabels,
					appsV1.DefaultDaemonSetUniqueLabelKey,
					"",
					namespace,
					nil)

				// start service watch
				dsm.serviceManager.Watch <- WatchData{
					ListOptions:  metaV1.ListOptions{TimeoutSeconds: &maxWatchTime, LabelSelector: labels.SelectorFromSet(daemonset.Spec.Selector.MatchLabels).String()},
					RegistryData: daemonsetData,
					Namespace:    daemonset.Namespace,
					Ctx:          ctx,
					LogEntry:     *daemonsetLog,
				}
			}
			daemonsetData.UpdateApplyStatus(daemonset.Status)
		case <-ctx.Done():
			daemonsetLog.Debug("Daemonset watcher was stopped. Got ctx done signal")
			watcher.Stop()
			return
		}
	}
}

// watchEvents will watch for events related to the Daemonset Resource
func (dsm *DaemonsetManager) watchEvents(ctx context.Context, lg log.Entry, daemonsetData *DaemonsetData, listOptions metaV1.ListOptions, namespace string) {
	lg.Info("Started the event watcher on daemonset events")

	watchData := WatchEvents{
		ListOptions: listOptions,
		Namespace:   namespace,
		Ctx:         ctx,
		LogEntry:    lg,
	}
	eventChan := dsm.eventManager.Watch(watchData)
	go func() {
		for {
			select {
			case event := <-eventChan:
				daemonsetData.UpdateDaemonsetEvents(event)
			case <-ctx.Done():
				lg.Info("Stop watch on daemonset events")
				return
			}
		}
	}()
}

// AddNewDaemonset add new daemonset under application
func (dsm *DaemonsetManager) AddNewDaemonset(data ApplyEvent, applicationRegistry *RegistryRow, desiredState int32) *DaemonsetData {

	log := applicationRegistry.Log()
	dd := &DaemonsetData{
		Metadata: MetaData{
			Name:         data.ApplyName,
			Namespace:    data.Namespace,
			Annotations:  data.Annotations,
			Metrics:      GetMetricsDataFromAnnotations(data.Annotations),
			Alerts:       GetAlertsDataFromAnnotations(data.Annotations),
			DesiredState: desiredState,
		},
		Pods:                    make(map[string]DeploymenPod, 0),
		ProgressDeadlineSeconds: GetProgressDeadlineApply(data.Annotations, dsm.maxDeploymentTime),
	}
	applicationRegistry.DBSchema.Resources.Daemonsets[data.ApplyName] = dd

	log.Info("Daemonset was associated to the application")

	return dd
}
