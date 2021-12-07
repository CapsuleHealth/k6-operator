package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/go-logr/logr"
	"github.com/grafana/k6-operator/api/v1alpha1"
	"github.com/grafana/k6-operator/pkg/cloud"
	"github.com/grafana/k6-operator/pkg/resources/jobs"
	"github.com/grafana/k6-operator/pkg/types"
	k6types "go.k6.io/k6/lib/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// k6 Cloud related vars
// Right now operator works with one test at a time so these should be safe.
var (
	testRunId string
	token     string
)

// InitializeJobs creates jobs that will run initial checks for distributed test if any are necessary
func InitializeJobs(ctx context.Context, log logr.Logger, k6 *v1alpha1.K6, r *K6Reconciler) (res ctrl.Result, err error) {
	log.Info("Initialize test")

	k6.Status.Stage = "initialization"
	if err = r.Client.Status().Update(ctx, k6); err != nil {
		log.Error(err, "Could not update status of custom resource")
		return
	}

	if cli := types.ParseCLI(&k6.Spec); cli.HasCloudOut {
		var (
			secrets    corev1.SecretList
			secretOpts = &client.ListOptions{
				// TODO: find out a better way to get namespace here
				Namespace: "k6-operator-system",
				LabelSelector: labels.SelectorFromSet(map[string]string{
					"k6cloud": "token",
				}),
			}
		)
		if err := r.List(ctx, &secrets, secretOpts); err != nil {
			log.Error(err, "Failed to load k6 Cloud token")
			return res, err
		}

		if len(secrets.Items) < 1 {
			err := fmt.Errorf("There are no secrets to hold k6 Cloud token")
			log.Error(err, err.Error())
			return res, err
		}

		if t, ok := secrets.Items[0].Data["token"]; !ok {
			err := fmt.Errorf("The secret doesn't have a field token for k6 Cloud")
			log.Error(err, err.Error())
			return res, err
		} else {
			token = string(t)
		}
		log.Info("Token for k6 Cloud was loaded.")

		var initializer *batchv1.Job
		if initializer, err = jobs.NewInitializerJob(k6, cli.ArchiveArgs); err != nil {
			return res, err
		}

		log.Info(fmt.Sprintf("Initializer job is ready to start with image `%s` and command `%s`",
			initializer.Spec.Template.Spec.Containers[0].Image, initializer.Spec.Template.Spec.Containers[0].Command))

		if err = ctrl.SetControllerReference(k6, initializer, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for the initialize job")
			return
		}

		if err = r.Create(ctx, initializer); err != nil {
			log.Error(err, "Failed to launch k6 test initializer")
			return
		}
		err = wait.PollImmediate(time.Second*5, time.Second*60, func() (done bool, err error) {
			var (
				listOpts = &client.ListOptions{
					Namespace: k6.Namespace,
					LabelSelector: labels.SelectorFromSet(map[string]string{
						"app":      "k6",
						"k6_cr":    k6.Name,
						"job-name": fmt.Sprintf("%s-initializer", k6.Name),
					}),
				}
				podList = &corev1.PodList{}
			)
			if err := r.List(ctx, podList, listOpts); err != nil {
				log.Error(err, "Could not list pods")
				return false, err
			}
			if len(podList.Items) < 1 {
				log.Info("No initializing pod found yet")
				return false, nil
			}

			// there should be only 1 initializer pod
			if podList.Items[0].Status.Phase != "Succeeded" {
				log.Info("Waiting for initializing pod to finish")
				return false, nil
			}

			// Here we need to get the output of the pod
			// pods/log is not currently supported by controller-runtime client and it is officially
			// recommended to use REST client instead:
			// https://github.com/kubernetes-sigs/controller-runtime/issues/1229

			var opts corev1.PodLogOptions
			config, err := rest.InClusterConfig()
			if err != nil {
				log.Error(err, "unable to fetch in-cluster REST config")
				// don't return here
				return false, nil
			}

			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				log.Error(err, "unable to get access to clientset")
				// don't return here
				return false, nil
			}
			req := clientset.CoreV1().Pods(k6.Namespace).GetLogs(podList.Items[0].Name, &opts)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
			defer cancel()

			podLogs, err := req.Stream(ctx)
			if err != nil {
				log.Error(err, "unable to stream logs from the pod")
				// don't return here
				return false, nil
			}
			defer podLogs.Close()

			buf := new(bytes.Buffer)
			_, err = io.Copy(buf, podLogs)
			if err != nil {
				log.Error(err, "unable to copy logs from the pod")
				return false, err
			}

			var execSpec struct {
				TotalDuration k6types.NullDuration `json:"totalDuration"`
				MaxVUs        uint64               `json:"maxVUs"`
			}

			if err := json.Unmarshal(buf.Bytes(), &execSpec); err != nil {
				return true, err
			}

			log.Info(fmt.Sprintf("Execution requirements: %+v", execSpec))

			if int32(execSpec.MaxVUs) < k6.Spec.Parallelism {
				err = fmt.Errorf("number of instances > number of VUs")
				// TODO maybe change this to a warning and simply set parallelism = maxVUs and proceed with execution?
				// But logr doesn't seem to have warning level by default, only with V() method...
				// It makes sense to return to this after / during logr VS logrus issue https://github.com/grafana/k6-operator/issues/84
				log.Error(err, "Parallelism argument cannot be larger than maximum VUs in the script",
					"maxVUs", execSpec.MaxVUs,
					"parallelism", k6.Spec.Parallelism)
				return false, err
			}

			if refID, err := cloud.CreateTestRun("k6-operator-test", token,
				execSpec.MaxVUs, execSpec.TotalDuration.TimeDuration().Seconds(),
				log); err != nil {
				return true, err
			} else {
				testRunId = refID
				log.Info(fmt.Sprintf("Created cloud test run: %s", testRunId))
				return true, nil
			}
		})
	}

	if err != nil {
		log.Error(err, "Failed to initialize script with cloud output")
		return
	}

	k6.Status.Stage = "initialized"
	if err = r.Client.Status().Update(ctx, k6); err != nil {
		log.Error(err, "Could not update status of custom resource")
		return
	}

	return res, nil
}
