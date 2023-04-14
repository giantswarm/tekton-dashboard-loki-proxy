[![CircleCI](https://circleci.com/gh/giantswarm/tekton-dashboard-loki-proxy.svg?style=shield)](https://circleci.com/gh/giantswarm/tekton-dashboard-loki-proxy)

# tekton-dashboard-loki-proxy chart

`tekton-dashboard-loki-proxy` acts as a proxy between [Tekton Dashboard](https://tekton.dev/docs/dashboard/) and [Loki](https://grafana.com/oss/loki/) so that Loki can be used as an `external-logs` source in Tekton Dashboard.

## Installing

There are several ways to install this app onto a Giant Swarm workload cluster.

- [Using GitOps to instantiate the App](https://docs.giantswarm.io/advanced/gitops/#installing-managed-apps)
- [Using our web interface](https://docs.giantswarm.io/ui-api/web/app-platform/#installing-an-app).
- By creating an [App resource](https://docs.giantswarm.io/ui-api/management-api/crd/apps.application.giantswarm.io/) in the management cluster as explained in [Getting started with App Platform](https://docs.giantswarm.io/app-platform/getting-started/).

Using Helm:

```sh
helm repo add giantswarm https://giantswarm.github.io/giantswarm-catalog

helm install my-tekton-dashboard-loki-proxy giantswarm/tekton-dashboard-loki-proxy --version 0.0.2
```

## Configuring

### values.yaml

**This is an example of a values file you could upload using our web interface.**

```yaml
# values.yaml
lokiEndpoint: "http://loki-read.loki:3100"
orgID: "1"
since: "120h"
```

Once installed you'll need to add similar to the following to your Tekton Dashboard arguments:

```
--external-logs=http://tekton-dashboard-loki-proxy.tekton-dashboard-loki-proxy:3000/logs
```

More details on using external logs with Tekton Dashboard can be found in the [Tekton Dashboard walk-through - Logs persistence](https://github.com/tektoncd/dashboard/blob/main/docs/walkthrough/walkthrough-logs.md).

### Sample App CR and ConfigMap for the management cluster

If you have access to the Kubernetes API on the management cluster, you could create
the App CR and ConfigMap directly.

Here is an example that would install the app to
workload cluster `abc12`:

```yaml
# appCR.yaml
apiVersion: application.giantswarm.io/v1alpha1
kind: App
metadata:
  name: tekton-dashboard-loki-proxy
  namespace: abc12
spec:
  catalog: giantswarm
  kubeConfig:
    inCluster: false
  name: tekton-dashboard-loki-proxy
  namespace: tekton-dashboard-loki-proxy
  userConfig:
    configMap:
      name: tekton-dashboard-loki-proxy-values
      namespace: abc12
  version: 0.0.2
```

```yaml
# user-values-configmap.yaml
apiVersion: v1
data:
  values: |
    lokiEndpoint: "http://loki-read.loki:3100"
kind: ConfigMap
metadata:
  name: tekton-dashboard-loki-proxy-values
  namespace: abc12
```

See our [full reference on how to configure apps](https://docs.giantswarm.io/app-platform/app-configuration/) for more details.
