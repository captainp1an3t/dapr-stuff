# Per-service ServiceAccounts and least-privilege secret access

Dapr's Helm chart automatically creates a `Role/secret-reader` and a `RoleBinding/dapr-secret-reader` in every namespace it operates in, binding the built-in `default` ServiceAccount to `get secrets`. This is convenient but silently broadens the privileges of every pod that runs as the default SA — including pods that have nothing to do with Dapr. We adopt the standard mitigation: each application gets its own dedicated ServiceAccount (`<svc>-sa`), Deployments set `spec.serviceAccountName` explicitly, and `get secrets` is granted only via _our own_ Role/RoleBinding scoped to the specific SA that needs it (currently `hello-svc-sa`; in T13 also `notifier-svc-sa`). The Dapr-created default-SA binding is left in place — removing it would fight the Helm chart and could break other Dapr features — but our workloads no longer use the `default` SA, so the auto-grant becomes moot for us.

## Considered alternatives

- **Do nothing.** Rely on the Helm-created `default` SA binding. Rejected: makes the demo model bad practice; production security review would flag it; using the `default` SA at all is a common security anti-pattern regardless of Dapr.
- **Override the Helm chart to disable `dapr-secret-reader`.** Rejected: fights the tool, adds a `values.yaml` override that has to be tracked across Dapr upgrades, and Dapr features may quietly assume the binding exists. Cleaner to just not use `default`.
- **Grant `get secrets` at the ClusterRole level.** Rejected: even wider than the Dapr default. Namespace-scoped Role is the right granularity for our single-namespace demo.

## Consequences

- Every new service manifest must declare a `ServiceAccount` and set `spec.serviceAccountName` on the Deployment. Adds ~6 lines of YAML per service.
- Secret-reading services must additionally get an explicit `Role` + `RoleBinding` for the specific secrets/namespaces they need. Kept close to the Deployment for cohesion.
- `kubectl auth can-i get secrets --as=system:serviceaccount:default:default` will still return `yes` because Dapr's helm-created binding is untouched. That's a Helm-chart-level issue, not an app-level one — flagged in NOTES for a future infrastructure-side follow-up.
- The pattern is directly copy-pasteable for real production services.
