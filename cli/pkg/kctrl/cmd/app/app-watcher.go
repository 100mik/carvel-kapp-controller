// Copyright 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cppforlife/color"
	"github.com/cppforlife/go-cli-ui/ui"
	uitable "github.com/cppforlife/go-cli-ui/ui/table"
	kcv1alpha1 "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apis/kappctrl/v1alpha1"
	kcclient "github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/clientset/versioned"
	kcexternalversions "github.com/vmware-tanzu/carvel-kapp-controller/pkg/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/tools/cache"
)

type AppWatcher struct {
	Namespace       string
	Name            string
	Follow          bool
	IgnoreNotExists bool

	ui     ui.UI
	client kcclient.Interface

	stopperChan chan struct{}
}

func NewAppWatcher(namespace string, name string, follow bool, ignoreIfExists bool, ui ui.UI, client kcclient.Interface) *AppWatcher {
	return &AppWatcher{Namespace: namespace, Name: name, Follow: follow, IgnoreNotExists: ignoreIfExists, ui: ui, client: client}
}

func (o *AppWatcher) printTillCurrent(status kcv1alpha1.AppStatus) (AppStage, error) {
	if status.Fetch != nil {
		if status.Fetch.ExitCode != 0 && status.Fetch.UpdatedAt.Unix() >= status.Fetch.StartedAt.Unix() {
			o.printLogLine("Fetch failed", status.Fetch.Stderr, true, nil)
			return fetchStage, fmt.Errorf(status.Fetch.Stderr)
		}
		if status.Fetch.StartedAt.After(status.Fetch.UpdatedAt.Time) {
			o.printLogLine("Fetch started", "", false, &status.Fetch.StartedAt.Time)
			return fetchStage, nil
		}
		o.printLogLine("Fetch succeeded", status.Fetch.Stdout, false, &status.Fetch.UpdatedAt.Time)
	}

	if status.Template != nil {
		if status.Template.ExitCode != 0 && status.Fetch.StartedAt.Unix() < status.Template.UpdatedAt.Unix() {
			o.printLogLine("Template failed", status.Template.Stderr, true, nil)
			return templateStage, fmt.Errorf(status.Template.Stderr)
		}
		if status.Fetch.StartedAt.After(status.Template.UpdatedAt.Time) {
			o.printLogLine("Template started", "", false, nil)
			return templateStage, nil
		}
		o.printLogLine("Template succeeded", "", false, &status.Template.UpdatedAt.Time)
	}

	if status.Deploy != nil {
		if status.Deploy.ExitCode != 0 && status.Deploy.StartedAt.Unix() < status.Deploy.UpdatedAt.Unix() {
			o.printLogLine("Deploy failed", status.Deploy.Stderr, true, nil)
			return deployStage, fmt.Errorf(status.Deploy.Error)
		}
		if o.hasReconciled(status) {
			o.printLogLine("Deploy succeeded", status.Deploy.Stdout, false, &status.Deploy.UpdatedAt.Time)
			return reconciled, nil
		}
		o.printLogLine("Deploy started", status.Deploy.Stdout, false, &status.Deploy.StartedAt.Time)
	}

	return "", nil
}

func (o *AppWatcher) printUpdate(oldStatus kcv1alpha1.AppStatus, status kcv1alpha1.AppStatus) {
	if status.Fetch != nil {
		if oldStatus.Fetch == nil || (!oldStatus.Fetch.StartedAt.Equal(&status.Fetch.StartedAt) && status.Fetch.UpdatedAt.Unix() <= status.Fetch.StartedAt.Unix()) {
			o.printLogLine("Fetch started", "", false, nil)
		}
		if oldStatus.Fetch == nil || !oldStatus.Fetch.UpdatedAt.Equal(&status.Fetch.UpdatedAt) {
			if status.Fetch.ExitCode != 0 && status.Fetch.UpdatedAt.Unix() >= status.Fetch.StartedAt.Unix() {
				o.printLogLine("Fetch failed", status.Template.Stderr, true, nil)
				o.stopWatch()
			}
			o.printLogLine("Fetch succeeded", status.Fetch.Stdout, false, nil)
		}
	}
	if status.Template != nil {
		if oldStatus.Template == nil || !oldStatus.Template.UpdatedAt.Equal(&status.Template.UpdatedAt) {
			if status.Template.ExitCode != 0 {
				o.printLogLine("Template failed", status.Template.Stderr, true, nil)
				o.stopWatch()
			}
			o.printLogLine("Template succeeded", "", false, nil)
		}
	}
	if status.Deploy != nil {
		if oldStatus.Deploy == nil || !oldStatus.Deploy.StartedAt.Equal(&status.Deploy.StartedAt) {
			o.printLogLine("Deploy started", "", false, nil)
		}
		if oldStatus.Deploy == nil || !oldStatus.Deploy.UpdatedAt.Equal(&status.Deploy.UpdatedAt) {
			if status.Template.ExitCode != 0 && status.Deploy.Finished {
				o.printLogLine("Deploy failed", status.Deploy.Stderr, true, nil)
				o.stopWatch()
			}
			o.printLogLine("Deploy progressing", status.Deploy.Stdout, false, nil)
		}
	}
	if o.hasReconciled(status) {
		o.printLogLine("App reconciled", "", false, nil)
		o.stopWatch()
	}
}

func (o *AppWatcher) PrintTillCurrent(status kcv1alpha1.AppStatus) (AppStage, error) {
	return o.printTillCurrent(status)
}

func (o *AppWatcher) PrintInfo(app kcv1alpha1.App) {
	table := uitable.Table{
		Transpose: true,

		Header: []uitable.Header{

			uitable.NewHeader("Name"),
			uitable.NewHeader("Namespace"),
			uitable.NewHeader("Status"),
			uitable.NewHeader("Metrics"),
		},

		Rows: [][]uitable.Value{{
			uitable.NewValueString(app.Name),
			uitable.NewValueString(app.Namespace),
			uitable.NewValueString(o.statusString(app.Status)),
			uitable.NewValueString(o.metricString(app.Status)),
		}},
	}

	o.ui.PrintTable(table)
}

func (o *AppWatcher) metricString(status kcv1alpha1.AppStatus) string {
	if status.ConsecutiveReconcileFailures != 0 {
		return fmt.Sprintf("%d consecutive failures", status.ConsecutiveReconcileFailures)
	} else if status.ConsecutiveReconcileSuccesses != 0 {
		return fmt.Sprintf("%d consecutive successes", status.ConsecutiveReconcileSuccesses)
	} else {
		return "0 consecutive failures | 0 consecutive successes"
	}
}

// Needs to be ait tight
func (o *AppWatcher) statusString(status kcv1alpha1.AppStatus) string {
	switch status.Conditions[0].Type {
	case kcv1alpha1.Reconciling:
		return "Reconciling"
	case kcv1alpha1.ReconcileSucceeded:
		return color.GreenString("Reconcile succeeded")
	case kcv1alpha1.ReconcileFailed:
		return color.RedString("Reconcile failed")
	case kcv1alpha1.Deleting:
		return "Deleting"
	case kcv1alpha1.DeleteFailed:
		return color.RedString("Deletion failed")
	default:
		return status.FriendlyDescription
	}
}

func (o *AppWatcher) hasReconciled(status kcv1alpha1.AppStatus) bool {
	for _, condition := range status.Conditions {
		if condition.Type == kcv1alpha1.ReconcileSucceeded && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (o *AppWatcher) TailAppStatus(app *kcv1alpha1.App) error {
	lastStage, err := o.printTillCurrent(app.Status)
	if err != nil {
		return err
	}

	if lastStage == reconciled {
		return nil
	}

	informerFactory := kcexternalversions.NewFilteredSharedInformerFactory(o.client, 30*time.Minute, o.Namespace, func(opts *metav1.ListOptions) {
		opts.FieldSelector = fmt.Sprintf("metadata.name=%s", o.Name)
	})
	informer := informerFactory.Kappctrl().V1alpha1().Apps().Informer()

	o.stopperChan = make(chan struct{})
	//defer close(o.stopperChan)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: o.udpateEventhandler,
	})

	go informer.Run(o.stopperChan)
	if !cache.WaitForCacheSync(o.stopperChan, informer.HasSynced) {
		return fmt.Errorf("Timed out waiting for caches to sync")
	}
	<-o.stopperChan
	return nil
}

func (o *AppWatcher) stopWatch() {
	close(o.stopperChan)
}

func (o *AppWatcher) udpateEventhandler(oldObj interface{}, newObj interface{}) {
	newApp, _ := newObj.(*kcv1alpha1.App)
	oldApp, _ := oldObj.(*kcv1alpha1.App)

	o.printUpdate(oldApp.Status, newApp.Status)
}

func (o *AppWatcher) printLogLine(message string, messageBlock string, errorBlock bool, startTime *time.Time) {
	messageAge := ""
	if startTime != nil {
		messageAge = fmt.Sprintf("(%s ago)", duration.ShortHumanDuration(time.Since(*startTime)))
	}
	o.ui.BeginLinef("%s:%s %s\n", time.Now().Format("3:04:05PM"), message, messageAge)
	if len(messageBlock) > 0 {
		o.ui.PrintBlock([]byte(o.indentMessageBlock(messageBlock, errorBlock)))
	}
}

func (o *AppWatcher) indentMessageBlock(messageBlock string, errored bool) string {
	lines := strings.Split(messageBlock, "\n")
	for ind := range lines {
		if errored {
			lines[ind] = color.RedString(lines[ind])
		}
		lines[ind] = fmt.Sprintf("\t  | %s", lines[ind])
	}

	indentedBlock := strings.Join(lines, "\n")
	if strings.LastIndex(indentedBlock, "\n") != (len(indentedBlock) - 1) {
		indentedBlock += "\n"
	}
	return indentedBlock
}

func prettyPrint(i interface{}) string {
	s, _ := json.MarshalIndent(i, "", "\t")
	return string(s)
}
