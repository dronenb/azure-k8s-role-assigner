# azure-k8s-role-assigner
This Kubernetes controller watches for ClusterRoleBindings and RoleBindings, and assigns the groups in said object to the AzureAD Service Principal, so that the groups returned in the JWT token are filtered.
