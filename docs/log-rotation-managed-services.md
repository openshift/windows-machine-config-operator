# Log rotation for managed Windows services

Log rotation for managed Windows services is available for WMCO 10.22+. This feature rotates log files based
on configurable size and age thresholds and is configured via environment variables in the operator.

## Enabling log rotation for managed Windows services

To enable and customize the log rotation behavior, add the following environment variables to the subscription (OLMv0).
The operator will restart to load the newly added environment variables and apply log rotation to the
managed services. This will result in a reconfiguration of the existing Windows nodes, one at a time, until all
nodes have been handled, to minimize disruption.

### Setting environment variables in the subscription:
```yaml
kind: Subscription
spec:
  config:
    env:
      - name: SERVICES_LOG_FILE_SIZE
        value: "100M"  # Rotate when log reaches this size (suggested: 100M)
      - name: SERVICES_LOG_FILE_AGE
        value: "168h"  # Keep rotated logs for this duration (e.g: 168h/7 days)
      - name: SERVICES_LOG_FLUSH_INTERVAL
        value: "5s"    # Flush logs to disk at this interval (suggested: 5s)
```

### Patching the subscription using the CLI:
```shell script
oc patch subscription <subscription_name> -n <namespace_name> \
  --type=merge \
  -p '{"spec":{"config":{"env":[{"name":"SERVICES_LOG_FILE_SIZE","value":"100M"},{"name":"SERVICES_LOG_FILE_AGE","value":"168h"},{"name":"SERVICES_LOG_FLUSH_INTERVAL","value":"5s"}]}}}'
```

### Patching the operator deployment using the CLI (OLMv1 or manual installs):

```shell script
    oc set env deployment/windows-machine-config-operator -n <namespace_name> -c manager \
      SERVICES_LOG_FILE_SIZE="100M" \
      SERVICES_LOG_FILE_AGE="168h" \
      SERVICES_LOG_FLUSH_INTERVAL="5s"
```
where:
- `<namespace_name>`: The namespace where the operator is installed (e.g., `openshift-windows-machine-config-operator`)
- `<subscription_name>`: The name of the subscription used to install the operator (e.g., `windows-machine-config-operator-subscription`)

## Disabling log rotation for managed Windows services

To disable log rotation, remove the `SERVICES_LOG_FILE_SIZE`, `SERVICES_LOG_FILE_AGE`, and `SERVICES_LOG_FLUSH_INTERVAL`
environment variables from the subscription or operator deployment.

## Behavior when log rotation settings change

**Effect on existing log files:** When rotation settings are changed (enabled, disabled, or updated), any previously
rotated log files are retained according to the `SERVICES_LOG_FILE_AGE` value that was in effect when they were
created. Once that retention period expires, the files are cleaned up automatically. New log files and any future
rotated files will follow the updated rotation rules going forward.

**Operator and node behavior:** Any change to the `SERVICES_LOG_FILE_SIZE`, `SERVICES_LOG_FILE_AGE`, or
`SERVICES_LOG_FLUSH_INTERVAL` environment variables—whether in the subscription (OLMv0) or the operator deployment
(OLMv1 / manual installs)—will cause the operator to restart in order to load the updated configuration. After
restarting, the operator will reconfigure each Windows node one at a time to apply the new log rotation settings,
minimizing disruption. Note that service continuity during reconfiguration is not guaranteed; brief interruptions
to managed services (such as kubelet or kube-proxy) may occur on each node as it is reconfigured.
