/*
Copyright 2025.

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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os" // For reading Slack token from env or file
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes" // Import standard client-go
	"k8s.io/client-go/rest"       // Import rest config
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	// Import the Slack library
	"github.com/slack-go/slack"
)

const (
	// Annotation to mark jobs that have already been notified
	slackNotificationAnnotation = "ctrianta30/slack-notified-failed"
	// Environment variable for Slack token (use secrets in production!)
	slackTokenEnvVar = "SLACK_BOT_TOKEN"
	// Environment variable for Slack channel ID
	slackChannelEnvVar = "SLACK_CHANNEL_ID"
	// OwnerReference Kind for CronJob
	cronJobKind = "CronJob"
)

// CronjobAlertReconciler reconciles a CronjobAlert object
type CronjobAlertReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Config       *rest.Config // Inject rest.Config
	SlackClient  *slack.Client
	SlackChannel string
}

// +kubebuilder:rbac:groups=monitor.ctrianta30,resources=cronjobalerts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitor.ctrianta30,resources=cronjobalerts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitor.ctrianta30,resources=cronjobalerts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronjobAlert object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/reconcile
func (r *CronjobAlertReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log = log.WithValues("job", req.NamespacedName)

	// 1. Fetch the Job instance
	var job batchv1.Job
	if err := r.Get(ctx, req.NamespacedName, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// Job deleted, nothing to do.
			log.Info("Job resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Job")
		return ctrl.Result{}, err
	}

	// 2. Check if the Job has already been processed for failure notification
	annotations := job.GetAnnotations()
	if _, exists := annotations[slackNotificationAnnotation]; exists {
		log.Info("Job already processed for Slack notification", "annotation", slackNotificationAnnotation)
		return ctrl.Result{}, nil
	}

	// 3. Check if the Job is owned by a CronJob
	ownerRef := metav1.GetControllerOf(&job)
	if ownerRef == nil || ownerRef.Kind != cronJobKind {
		// Not owned by a CronJob, ignore
		// log.V(1).Info("Job is not owned by a CronJob, ignoring") // Use V(1) for less verbose logs
		return ctrl.Result{}, nil
	}
	cronJobName := ownerRef.Name
	log = log.WithValues("cronjob", cronJobName) // Add cronjob name to logs

	// 4. Check if the Job has failed
	// Conditions might appear in different orders, or take time to populate.
	// Check the Succeeded count and Active count as well for robustness.
	isFailed := false
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			isFailed = true
			break
		}
	}
    // Additional check: if job has >0 completions required, succeeded count is 0, and active count is 0, it might also imply failure
    // but the condition check is usually sufficient. Consider job backoff limits as well.

	if !isFailed {
		// Job hasn't failed (yet), or succeeded. Nothing to do for failures.
		// log.V(1).Info("Job has not failed.")
		return ctrl.Result{}, nil
	}

	log.Info("Detected failed Job owned by CronJob")

	// --- Job Failed, Owned by CronJob, and Not Yet Notified ---

	// 5. Get Pods associated with the Job
	pods, err := r.getJobPods(ctx, &job)
	if err != nil {
		log.Error(err, "Failed to get pods for job")
		// Requeue might be appropriate if listing pods temporarily failed
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if len(pods.Items) == 0 {
		log.Info("No pods found for failed job yet, will retry if needed")
		// Pods might not be created/visible yet, requeue might be desired
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 6. Fetch Logs from the first Pod (often sufficient for failure)
	// In more complex scenarios, you might want logs from all failed pods.
	podLog := "Could not fetch logs." // Default message
	clientset, err := kubernetes.NewForConfig(r.Config) // Create standard clientset
	if err != nil {
		log.Error(err, "Failed to create kubernetes clientset")
		// This is a configuration error, likely non-recoverable without restart
		return ctrl.Result{}, err
	}

	// Get logs from the first pod found
	firstPod := pods.Items[0]
	log = log.WithValues("pod", firstPod.Name)
	log.Info("Fetching logs from pod")

	logOptions := &corev1.PodLogOptions{
		// You might want to customize this (e.g., TailLines, SinceSeconds)
		TailLines: &[]int64{100}[0], // Get last 100 lines
	}
	reqLog := clientset.CoreV1().Pods(firstPod.Namespace).GetLogs(firstPod.Name, logOptions)
	podLogsStream, err := reqLog.Stream(ctx)
	if err != nil {
		log.Error(err, "Failed to stream pod logs")
		podLog = fmt.Sprintf("Error fetching logs: %s", err.Error())
	} else {
		defer podLogsStream.Close()
		buf := new(bytes.Buffer)
		_, err := io.Copy(buf, podLogsStream)
		if err != nil {
			log.Error(err, "Failed to copy pod logs stream")
			podLog = fmt.Sprintf("Error reading logs stream: %s", err.Error())
		} else {
			podLog = buf.String()
			if len(podLog) > 3000 { // Slack messages have limits
				podLog = podLog[len(podLog)-3000:] // Truncate logs if too long
				podLog = "...\n" + podLog         // Indicate truncation
			}
		}
	}

	// 7. Send Slack Notification
	if r.SlackClient == nil || r.SlackChannel == "" {
		log.Error(err, "Slack client or channel not configured. Cannot send notification.")
        // Mark as notified to prevent retries if Slack isn't set up
        // Or return an error if you want it to keep retrying
	} else {
		message := fmt.Sprintf("🚨 *CronJob Failure Detected* 🚨\n\n*CronJob:* `%s`\n*Failed Job:* `%s`\n*Namespace:* `%s`\n*Failed Pod:* `%s`\n\n*Logs (last 100 lines):*\n```%s```",
			cronJobName,
			job.Name,
			job.Namespace,
			firstPod.Name, // Assuming the first pod is relevant
			podLog,
		)

		log.Info("Sending notification to Slack channel", "channel", r.SlackChannel)
		_, timestamp, err := r.SlackClient.PostMessageContext(
			ctx,
			r.SlackChannel,
			slack.MsgOptionText(message, false),
			slack.MsgOptionAsUser(true), // Or false depending on your token type and preference
		)
		if err != nil {
			log.Error(err, "Failed to send Slack message")
			// Requeue to retry sending the notification later
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		log.Info("Successfully sent Slack message", "timestamp", timestamp)
	}

	// 8. Add annotation to the Job to prevent re-notification
	log.Info("Adding notification annotation to job")
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[slackNotificationAnnotation] = "true"
	job.SetAnnotations(annotations)

	if err := r.Update(ctx, &job); err != nil {
		log.Error(err, "Failed to update Job with notification annotation")
		// Requeue to retry updating the annotation
		return ctrl.Result{}, err // Use default retry backoff
	}

	log.Info("Successfully processed failed job and sent notification (or marked as processed)")
	return ctrl.Result{}, nil
}

// getJobPods fetches Pods controlled by a given Job
func (r *CronjobAlertReconciler) getJobPods(ctx context.Context, job *batchv1.Job) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	// Jobs use a 'controller-uid' label in their pod templates to select pods.
	// Alternatively, use the job's selector if defined, but controller-uid is common.
	labelSelector := labels.SelectorFromSet(map[string]string{"controller-uid": string(job.UID)})
	// Fallback or alternative: use job-name label if controller-uid isn't present/reliable
	// labelSelector := labels.SelectorFromSet(map[string]string{"job-name": job.Name})

	listOptions := &client.ListOptions{
		Namespace:     job.Namespace,
		LabelSelector: labelSelector,
	}
	err := r.List(ctx, podList, listOptions)
	return podList, err
}


// SetupWithManager sets up the controller with the Manager.
func (r *CronjobAlertReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Get Slack Token and Channel from environment variables
	// **IMPORTANT**: For production, mount these from a Kubernetes Secret!
	slackToken := os.Getenv(slackTokenEnvVar)
	slackChannel := os.Getenv(slackChannelEnvVar)

	if slackToken == "" {
		ctrl.Log.Info("Warning: SLACK_BOT_TOKEN environment variable not set. Slack notifications disabled.")
		// Handle appropriately - maybe disable reconciliation or log errors
	}
	if slackChannel == "" {
		ctrl.Log.Info("Warning: SLACK_CHANNEL_ID environment variable not set. Slack notifications disabled.")
		// Handle appropriately
	}

	// Initialize Slack Client
	// Handle case where token might be empty
	var slackClient *slack.Client
	if slackToken != "" {
		slackClient = slack.New(slackToken)
		_, err := slackClient.AuthTest() // Verify token is valid
        if err != nil {
            ctrl.Log.Error(err, "Slack Authentication failed. Check SLACK_BOT_TOKEN. Notifications may fail.")
            // Decide if you want the controller to crash or just log the error.
            // return fmt.Errorf("Slack auth failed: %w", err) // Option: stop controller startup
        } else {
			ctrl.Log.Info("Slack client initialized and authenticated successfully.")
		}
	}


	// Assign Slack client and channel to the reconciler instance
	r.SlackClient = slackClient
	r.SlackChannel = slackChannel

	// Store the rest.Config (needed for creating the standard clientset for logs)
	r.Config = mgr.GetConfig()


	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.Job{}).
		// Owns(&corev1.Pod{}). // Usually not needed; we list pods based on Job labels
		// WithEventFilter(pred). // Uncomment to use the predicate
		Complete(r)
}


