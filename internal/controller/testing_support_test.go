package controller

import (
	"context"
	"errors"
	"sync"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// errGroupNotFound is returned by the fake when a group is configured as unknown.
var errGroupNotFound = errors.New("group not found in Azure")

// fakeAzureManager is an in-memory implementation of AzureGroupManager used by
// unit tests. It records assignment and removal calls and lets tests seed the
// set of groups that are "already assigned" in Azure, as well as inject errors.
type fakeAzureManager struct {
	mu sync.Mutex

	// assigned is the set of group IDs currently assigned in Azure. It seeds
	// ListManagedAssignedGroupIDs and is updated by Assign/Remove calls so the
	// fake behaves like a real, idempotent backend.
	assigned map[string]struct{}

	// assignCalls and removeCalls record the group IDs passed to the respective
	// methods, in call order, for assertions.
	assignCalls []string
	removeCalls []string

	// unknownGroups are group IDs for which GetGroupByID should fail (simulating
	// a group that does not exist in Azure).
	unknownGroups map[string]struct{}

	// nilIDGroups are group IDs for which GetGroupByID returns a group whose ID
	// is nil (simulating a malformed Graph response).
	nilIDGroups map[string]struct{}

	// assignErr, if set, is returned by AssignGroupToServicePrincipals for the
	// matching group ID.
	assignErr map[string]error
	// removeErr, if set, is returned by RemoveGroupFromServicePrincipals for the
	// matching group ID.
	removeErr map[string]error

	// listErr, if set, is returned by ListManagedAssignedGroupIDs.
	listErr error
}

func newFakeAzureManager(alreadyAssigned ...string) *fakeAzureManager {
	f := &fakeAzureManager{
		assigned:      make(map[string]struct{}),
		unknownGroups: make(map[string]struct{}),
		nilIDGroups:   make(map[string]struct{}),
		assignErr:     make(map[string]error),
		removeErr:     make(map[string]error),
	}
	for _, id := range alreadyAssigned {
		f.assigned[id] = struct{}{}
	}
	return f
}

func (f *fakeAzureManager) GetGroupByID(_ context.Context, groupID string) (models.Groupable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.unknownGroups[groupID]; ok {
		return nil, errGroupNotFound
	}

	group := models.NewGroup()
	if _, ok := f.nilIDGroups[groupID]; !ok {
		id := groupID
		group.SetId(&id)
	}
	return group, nil
}

func (f *fakeAzureManager) AssignGroupToServicePrincipals(_ context.Context, groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.assignCalls = append(f.assignCalls, groupID)
	if err, ok := f.assignErr[groupID]; ok {
		return err
	}
	f.assigned[groupID] = struct{}{}
	return nil
}

func (f *fakeAzureManager) RemoveGroupFromServicePrincipals(_ context.Context, groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removeCalls = append(f.removeCalls, groupID)
	if err, ok := f.removeErr[groupID]; ok {
		return err
	}
	delete(f.assigned, groupID)
	return nil
}

func (f *fakeAzureManager) ListManagedAssignedGroupIDs(_ context.Context) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make(map[string]struct{}, len(f.assigned))
	for id := range f.assigned {
		out[id] = struct{}{}
	}
	return out, nil
}

// assignedList returns a sorted snapshot of currently-assigned group IDs.
func (f *fakeAzureManager) assignedSet() map[string]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.assigned))
	for id := range f.assigned {
		out[id] = struct{}{}
	}
	return out
}

// testScheme returns a runtime scheme registered with the core Kubernetes types
// (including rbac/v1) used by the reconcilers.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// roleBinding builds a namespaced RoleBinding with Group subjects for the given
// group IDs.
func roleBinding(namespace, name string, groupIDs ...string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Subjects:   groupSubjects(groupIDs...),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
	}
}

// clusterRoleBinding builds a ClusterRoleBinding with Group subjects for the
// given group IDs.
func clusterRoleBinding(name string, groupIDs ...string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   groupSubjects(groupIDs...),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
	}
}

func groupSubjects(groupIDs ...string) []rbacv1.Subject {
	subjects := make([]rbacv1.Subject, 0, len(groupIDs))
	for _, id := range groupIDs {
		subjects = append(subjects, rbacv1.Subject{Kind: "Group", Name: id})
	}
	return subjects
}

// metaNow returns the current time as a metav1.Time, for setting deletion
// timestamps in tests.
func metaNow() metav1.Time {
	return metav1.Now()
}
