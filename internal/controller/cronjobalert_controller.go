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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/slack-go/slack"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Annotation to mark jobs that have already been notified
	slackNotificationAnnotation = "ctrianta30/slack-notified-failed"
	// Annotation on the CRONJOB to store the last notification timestamp
	lastSlackNotificationTimestampAnnotation = "ctrianta30/last-slack-failure-notification-ts"
	// Environment variable for Slack token
	slackTokenEnvVar = "SLACK_BOT_TOKEN"
	// Environment variable for Slack channel ID
	slackChannelEnvVar = "SLACK_CHANNEL_ID"
	// OwnerReference Kind for CronJob
	cronJobKind = "CronJob"
	// Duration to wait before sending another notification for the same CronJob
	notificationRateLimitDurationEnvVar = "NOTIFICATION_RATE_LIMIT_DURUATION"
)

// CronjobAlertReconciler reconciles a CronjobAlert object
type CronjobAlertReconciler struct {
	client.Client
	Scheme                        *runtime.Scheme
	Config                        *rest.Config
	SlackClient                   *slack.Client
	SlackChannel                  string
	NotificationRateLimitDuration time.Duration
}

var slackBackoff = wait.Backoff{
	Steps:    5,
	Duration: 500 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watc
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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
	jobAnnotations := job.GetAnnotations()
	if _, exists := jobAnnotations[slackNotificationAnnotation]; exists {
		log.Info("Job already processed for Slack notification", "annotation", slackNotificationAnnotation)
		return ctrl.Result{}, nil
	}

	// 3. Check if the Job is owned by a CronJob
	ownerRef := metav1.GetControllerOf(&job)
	if ownerRef == nil || ownerRef.Kind != cronJobKind {
		// Not owned by a CronJob, ignore
		log.Info("Job is not owned by a CronJob, ignoring")
		return ctrl.Result{}, nil
	}
	cronJobName := ownerRef.Name
	cronJobNamespacedName := types.NamespacedName{Namespace: req.Namespace, Name: cronJobName}
	log = log.WithValues("cronjob", cronJobName)

	// 4. Check if the Job has failed
	// Check the Succeeded count and Active count as well for robustness.
	isFailed := false
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			isFailed = true
			break
		}
	}

	if !isFailed {
		// Job hasn't failed (yet), or succeeded. Nothing to do for failures.
		return ctrl.Result{}, nil
	}

	log.Info("Detected failed Job owned by CronJob")

	// --- Job Failed, Owned by CronJob, and Not Yet Notified ---

	// 5. Fetch the parent CronJob resource
	var cronJob batchv1.CronJob
	if err := r.Get(ctx, cronJobNamespacedName, &cronJob); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "Owner CronJob not found, cannot check rate limit or update timestamp.", "cronjob", cronJobNamespacedName)
		} else {
			log.Error(err, "Failed to get owner CronJob", "cronjob", cronJobNamespacedName)
			return ctrl.Result{}, err
		}
	} else {
		// CronJob fetched successfully, proceed with rate limit check
		cronJobAnnotations := cronJob.GetAnnotations()
		if lastTimestampStr, exists := cronJobAnnotations[lastSlackNotificationTimestampAnnotation]; exists {
			// Try parsing the stored timestamp
			lastTimestamp, err := time.Parse(time.RFC3339, lastTimestampStr)
			if err != nil {
				log.Error(err, "Failed to parse last notification timestamp annotation from CronJob", "annotationValue", lastTimestampStr)
				// Decide how to handle parse error - proceed with notification this time?
			} else {
				// Successfully parsed, check the time difference
				if time.Since(lastTimestamp) < r.NotificationRateLimitDuration {
					// It's been less than the rate limit duration since the last notification for this CronJob
					log.Info("Skipping Slack notification due to rate limiting",
						"cronjob", cronJobNamespacedName,
						"lastNotification", lastTimestamp,
						"rateLimit", r.NotificationRateLimitDuration)

					// *** IMPORTANT ***: Even though we skip Slack, we MUST still annotate the JOB
					// to prevent this specific Job failure from being re-processed indefinitely.
					if jobAnnotations == nil {
						jobAnnotations = make(map[string]string)
					}
					jobAnnotations[slackNotificationAnnotation] = "true"
					job.SetAnnotations(jobAnnotations)
					if err := r.Update(ctx, &job); err != nil {
						log.Error(err, "Failed to update Job annotation after skipping rate-limited notification")
						return ctrl.Result{}, err
					}
					log.Info("Added annotation to Job even though notification was rate-limited.")
					return ctrl.Result{}, nil
				}
			}
		}
	}

	// 6. Get Pods associated with the Job
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

	// 7. Fetch Logs from the first Pod (often sufficient for failure)
	podLog := "Could not fetch logs."
	clientset, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		log.Error(err, "Failed to create kubernetes clientset")
		return ctrl.Result{}, err
	}

	// Get logs from the first pod found
	firstPod := pods.Items[0]
	log = log.WithValues("pod", firstPod.Name)
	log.Info("Fetching logs from pod")

	logOptions := &corev1.PodLogOptions{
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
			if len(podLog) > 3000 {
				podLog = podLog[len(podLog)-3000:]
				podLog = "...\n" + podLog
			}
		}
		log.Info("Pod logs", "logs", podLog)
	}

	message := fmt.Sprintf("🚨 *CronJob Failure Detected* 🚨\n\n*CronJob:* `%s`\n*Failed Job:* `%s`\n*Namespace:* `%s`\n*Failed Pod:* `%s`\n\n*Logs (last 100 lines):*\n```%s```",
		cronJobName,
		job.Name,
		job.Namespace,
		firstPod.Name, // Assuming the first pod is relevant
		podLog,
	)

	// 8. Send Slack Notification
	err = r.sendSlackNotificationWithRetry(ctx, r.SlackChannel, message)

	// 9. Add annotation to the CronJob to prevent spam notifications
	var latestCronJob batchv1.CronJob
	if err := r.Get(ctx, cronJobNamespacedName, &latestCronJob); err != nil {
		log.Error(err, "Failed to re-fetch CronJob before updating timestamp annotation", "cronjob", cronJobNamespacedName)
		// Cannot update timestamp, but proceed to annotate the Job
	} else {
		nowStr := time.Now().Format(time.RFC3339)
		cronJobAnnos := latestCronJob.GetAnnotations()
		if cronJobAnnos == nil {
			cronJobAnnos = make(map[string]string)
		}
		cronJobAnnos[lastSlackNotificationTimestampAnnotation] = nowStr
		latestCronJob.SetAnnotations(cronJobAnnos)
		if err := r.Update(ctx, &latestCronJob); err != nil {
			// Handle potential update conflict or other errors
			if apierrors.IsConflict(err) {
				log.Info("Conflict updating CronJob timestamp annotation, will retry reconcile", "cronjob", cronJobNamespacedName)
				// Don't annotate the Job yet, requeue the whole thing to try again
				return ctrl.Result{Requeue: true}, nil
			}
			log.Error(err, "Failed to update CronJob with last notification timestamp annotation", "cronjob", cronJobNamespacedName)
			// Log error but proceed to annotate Job
		} else {
			log.Info("Successfully updated CronJob with last notification timestamp", "cronjob", cronJobNamespacedName, "timestamp", nowStr)
		}
	}

	// 10. Add annotation to the Job to prevent re-notification
	log.Info("Adding notification annotation to job")
	if jobAnnotations == nil {
		jobAnnotations = make(map[string]string)
	}
	jobAnnotations[slackNotificationAnnotation] = "true"
	job.SetAnnotations(jobAnnotations)

	if err := r.Update(ctx, &job); err != nil {
		log.Error(err, "Failed to update Job with notification annotation")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	log.Info("Successfully processed failed job and sent notification (or marked as processed)")
	return ctrl.Result{}, nil
}

// Function to wrap the Slack sending logic with retry
func (r *CronjobAlertReconciler) sendSlackNotificationWithRetry(ctx context.Context, channel string, message string) error {
	log := log.FromContext(ctx) // Get logger from context

	// Check SlackClient is configured before attempting
	if r.SlackClient == nil || r.SlackChannel == "" {
		log.Error(nil, "Slack client or channel not configured. Cannot send notification.")
		return errors.New("slack client not configured")
	}

	err := wait.ExponentialBackoff(slackBackoff, func() (bool, error) {
		log.Info("Attempting to send notification to Slack channel", "channel", channel)
		_, timestamp, err := r.SlackClient.PostMessageContext(
			ctx,
			channel,
			slack.MsgOptionText(message, false),
			slack.MsgOptionAsUser(true), // Or false
		)

		if err != nil {
			log.Error(err, "Failed to send Slack message")
			var rateLimitErr *slack.RateLimitedError
			if errors.As(err, &rateLimitErr) {
				// Slack Rate Limit Error - Retry after the suggested duration
				log.Info("Slack rate limited, will retry after suggested duration", "retryAfter", rateLimitErr.RetryAfter)
				return false, nil
			}

			// Check for known permanent errors based on error message text
			errMsg := err.Error()
			if errMsg == "missing_scope" || errMsg == "channel_not_found" || errMsg == "invalid_auth" || errMsg == "account_inactive" {
				log.Error(err, "Permanent Slack error, not retrying")
				return false, err
			}

			// Assume other errors
			log.Error(err, "Transient error sending Slack message, retrying...")
			return false, nil
		}

		log.Info("Successfully sent Slack message", "timestamp", timestamp)

		return true, nil
	})

	// Handle the final result of the retry loop
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) {
			log.Error(err, "Failed to send Slack message after multiple retries (timeout)")
			return fmt.Errorf("failed to send Slack message after %d retries: %w", slackBackoff.Steps, err)
		}
		// This was a non-retryable error returned from the condition function
		log.Error(err, "Failed to send Slack message due to permanent error")
		return fmt.Errorf("failed to send Slack message: %w", err)
	}

	return nil
}

// getJobPods fetches Pods controlled by a given Job
func (r *CronjobAlertReconciler) getJobPods(ctx context.Context, job *batchv1.Job) (*corev1.PodList, error) {
	podList := &corev1.PodList{}

	labelSelector := labels.SelectorFromSet(map[string]string{"controller-uid": string(job.UID)})

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
	slackToken := os.Getenv(slackTokenEnvVar)
	slackChannel := os.Getenv(slackChannelEnvVar)

	notificationRateLimitDurationStr := os.Getenv(notificationRateLimitDurationEnvVar)
	notificationRateLimitDurationInt, err := strconv.Atoi(notificationRateLimitDurationStr)
	if err != nil {
		// Handle the error appropriately, e.g., log it and use a default value
		fmt.Fprintf(os.Stderr, "Error converting %s to integer: %v\n", notificationRateLimitDurationEnvVar, err)
		// Use a default value of 5 minutes
		r.NotificationRateLimitDuration = 5 * time.Minute
	} else {
		r.NotificationRateLimitDuration = time.Duration(notificationRateLimitDurationInt) * time.Minute
	}

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
		_, err := slackClient.AuthTest()
		if err != nil {
			ctrl.Log.Error(err, "Slack Authentication failed. Check SLACK_BOT_TOKEN. Notifications may fail.")
		} else {
			ctrl.Log.Info("Slack client initialized and authenticated successfully.")
		}
	}

	r.SlackClient = slackClient
	r.SlackChannel = slackChannel

	r.Config = mgr.GetConfig()

	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.Job{}).
		Complete(r)
}
