# prune

```
cli-utils prune <dir>
```
can prune obsolete resources from a kustomization directory or resources that are with declarative deletion annotation.

## prune kustomize directory
To use prune for a kustomization directory, add an `inventory` field in `kustomization.yaml`. Here is an example:
```
inventory:
  type: ConfigMap
  configMap:
    name: prune-cm-name
    namespace: some-namespace
```
Then the output of the kustomization directory will contain a ConfigMap resource named `prune-cm-name`, which contains a list of all resources in this kustomization directory. 

When resources are removed from this kustomization directory, the `prune-cm-name` will contains a different list of resources. The `prune` compares the old list with the new list to get a diff and deletes the obsolete resources.

The [hello](../config/helloWithInventory) example contains a `kustomization.yaml` with an `inventory` field. Apply it by
```
cli-utils apply config/helloWithInventory
```

The output is like the following
```
Doing `cli-utils apply`
applied ConfigMap/prune-cm-name
applied Pod/myapp-pod
applied ConfigMap/example-cfgmap
applied StatefulSet/web
applied Deployment/frontend
Resources: 5
```

Remove the `pod.yaml` from the resources list in `kustomization.yaml` and apply the directory again.
```
cli-utils apply config/helloWithInventory
```

The output is like the following
```
Doing `cli-utils apply`
applied ConfigMap/prune-cm-name
applied ConfigMap/example-cfgmap
applied StatefulSet/web
applied Deployment/frontend
Resources: 4
```

So far the `Pod` object has been deployed from the cluster.
It's still running and you can verify that by
```
kubectl get pod
```

The output is like the following
```
NAME                                           READY   STATUS    RESTARTS   AGE
myapp-pod                                      1/1     Running   0         1m
```

Then run the prune subcommand by
```
cli-utils prune config/helloWithInventory
```

The output is like the following
```
Doing `cli-utils prune`
Resources: 1
```

Verify that the Pod object has been deleted by

```
kubectl get pod
```
The Pod `myapp-pod` is not in the output list.


Delete the applied resources by
```
cli-utils delete config/helloWithInventory
```

## prune through declarative deletion

If you don't have a kustomization directory, but still want to prune
resources, you can apply declarative deletion annotation to those
resources. Then cli-utils is aware that they are intended to be pruned.

The [manifests](../config/manifests) contains three resources. Apply them by

```
cli-utils apply config/manifests
```

Say the Pod object need to be pruned, you can add the following annotation to `config/manifests/pod.yaml`:

```
annotations:
  kubectl.kubernetes.io/presence: EnsureDoesNotExist
```

Apply again by
```
cli-utils apply config/manifests
```

The Pod `myapp-pod` is still running after apply. Run `prune` subcommand to clean it up.
```
cli-utils prune config/manifests
```

The output is like the following
```
Doing `cli-utils prune`
Resources: 1
```

Delete all of the applied resources by
```
cli-utils delete config/manifests
```
