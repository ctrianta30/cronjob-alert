# Kubernetes CronJob Failure Slack Notifier

## Overview

This Kubernetes controller monitors `batch/v1.Job` resources within a cluster. When it detects that a Job created and owned by a `batch/v1.CronJob` has failed, it sends a detailed notification to a configured Slack channel. The notification includes information about the CronJob, the failed Job, the Pod, the namespace, and the last 100 lines of logs from the failed Pod to aid in debugging.

The controller incorporates features like idempotency (to avoid duplicate alerts for the same Job instance), rate limiting (to prevent alert storms from frequently failing CronJobs), and configurable behavior per CronJob.

Built with [Kubebuilder](https://book.kubebuilder.io/).

## Features

* **Watches Job Failures:** Monitors `batch/v1.Job` resources across the cluster.
* **CronJob Focus:** Filters for Jobs specifically owned by `batch/v1.CronJob` resources.
* **Slack Notifications:** Sends alerts to a designated Slack channel upon failure detection.
* **Detailed Alerts:** Notifications include CronJob, Job, Pod name, Namespace, and truncated Pod logs.
* **Pod Log Retrieval:** Fetches the last 100 lines of logs from the failed Job's Pod.
* **Retry Logic:** Implements exponential backoff when attempting to send Slack messages to handle transient network errors.
* **Idempotency:** Prevents duplicate notifications for the *exact same Job failure* by annotating processed Jobs (`ctrianta30/slack-notified-failed`).
* **Rate Limiting:** Prevents alert spam by limiting notifications for failures originating from the *same CronJob* based on a configurable time window.
    * Tracks the last notification time per CronJob via an annotation (`ctrianta30/last-slack-failure-notification-ts`).
    * Uses a default rate limit configurable via an environment variable.
    * Allows overriding the rate limit per CronJob via an annotation (`ctrianta30/notification-rate-limit-minutes`).

## Prerequisites

* Kubernetes Cluster (v1.19+)
* `kubectl` configured to access the cluster.
* Docker (for building the container image).
* `make` (for using the Makefile commands).
* Go (if modifying the code, check Kubebuilder docs for version compatibility).
* A Slack Workspace and permission to create/manage Slack Apps.

## Configuration

The controller requires Slack API credentials and configuration for rate limiting.

### 1. Slack Credentials (Kubernetes Secret)

The controller expects a Slack Bot Token and a target Channel ID to be provided via a Kubernetes Secret.

**Slack App Setup:**
1.  Create a Slack App at [https://api.slack.com/apps](https://api.slack.com/apps).
2.  Navigate to "OAuth & Permissions".
3.  Under "Bot Token Scopes", add the `chat:write` scope.
4.  Install the app to your workspace.
5.  Copy the "Bot User OAuth Token" (starts with `xoxb-`).
6.  Identify the Channel ID you want notifications sent to (you can find this in the channel's URL or via other methods).

**Create the Secret:**
Replace placeholders and run the following command (ensure the namespace matches where you deploy the controller, typically `<project-name>-system` like `cronjob-alerter-system`):

```bash
kubectl create secret generic slack-credentials \
  --from-literal=SLACK_BOT_TOKEN='<your-bot-token>' \
  --from-literal=SLACK_CHANNEL_ID='<your-channel-id>' \
  -n cronjob-alerter-system # Adjust namespace if needed
```

Or create using YAML:

```
# slack-credentials.yaml
apiVersion: v1
kind: Secret
metadata:
  name: slack-credentials
  namespace: cronjob-alert
type: Opaque
stringData:
data:
  SLACK_BOT_TOKEN: "<base64 token>"
  SLACK_CHANNEL_ID: "<base64 slack channel id>"
```

Apply with `kubectl apply -f slack-credentials.yaml`

### 2. Environment Variables

The controller's Deployment mounts the `slack-credentials` secret as environment variables:

- SLACK_BOT_TOKEN: Used to authenticate with the Slack API.
- SLACK_CHANNEL_ID: The ID of the channel where notifications will be sent.

### 3. CronJob Annotations (Per-CronJob Configuration)

You can customize the rate limiting behavior for individual CronJobs using annotations in their metadata:

- `ctrianta30/notification-rate-limit-minutes`: (Optional) Set this annotation on a CronJob to prevent alert storms from frequently failing CronJobs .
    - Value must be a string representing a positive integer (e.g., "10" for 10 minutes, "60" for 1 hour).
    - If the annotation is missing, the controller will send a notification for every failure.

**Example Cronjob**

```
# cronjob.yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: fail-cronjob-test
  annotations:
    ctrianta30/notification-rate-limit-minutes: "2"
spec:
  schedule: "*/1 * * * *" # Run every minute for testing
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: fail-container
            image: busybox
            command: ["/bin/sh", "-c", "echo 'Simulating failure'; exit 1"]
          restartPolicy: Never
      backoffLimit: 1
```

- `ctrianta30/last-slack-failure-notification-ts`: Managed by the controller. This annotation is automatically added/updated by the controller on the CronJob resource to store the timestamp (RFC3339 format) of the last successful Slack notification sent for that CronJob. Users should not set this manually.

### 4. Job Annotation (Controller Managed)

- `ctrianta30/slack-notified-failed`: Managed by the controller. This annotation is added to Job resources after a failure notification has been successfully processed (either sent or skipped due to rate limiting). This prevents the controller from reprocessing the exact same Job instance if a reconcile loop is triggered again for it.

## Deployment

### 1. Build and Push the Image:

```
# Replace with your container registry path
export IMG="your-registry/cronjob-alert:latest"
make docker-build docker-push IMG=$IMG
```

### 2. Create the Slack Secret:

Ensure you have created the `slack-credentials` secret in the target namespace as described in the Configuration section.

### 3. Deploy the Controller:

This command applies the necessary Custom Resource Definitions (if any were defined, though this controller doesn't strictly need one), RBAC rules (ClusterRole, ClusterRoleBinding), and the Deployment for the controller manager.

```
make deploy IMG=$IMG
```

## RBAC Permissions

The controller requires the following permissions (defined via `//+kubebuilder:rbac` markers and applied by `make deploy`):

- batch/v1:
    - Jobs: `get`, `list`, `watch`, `update`, `patch`, `delete`
    - Jobs/status: `get`, `update`, `patch`
    - Jobs/finalizers: `update`
    - CronJobs: `get`, `list`, `watch`, `update`

- core/v1 (""):
    - Pods: `get`, `list`, `watch`
    - Pods/log: `get`
    - Secrets: `get`, `list`, `watch`
    - Events: `create`, `patch`

- coordination.k8s.io/v1:
    - Leases: `get`, `list`, `watch`, `create`, `update`, `patch`, `delete`

## Development

This controller is built using Kubebuilder v3+.

- **Run Locally**: `make run ENABLE_WEBHOOKS=false` (Requires Go installed and environment variables SLACK_BOT_TOKEN and SLACK_CHANNEL_ID).
- **Generate Manifests**: `make manifests`
- **Run Tests**: `make test`