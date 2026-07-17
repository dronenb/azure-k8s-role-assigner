package azure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	appmetrics "github.com/dronenb/azure-k8s-role-assigner/internal/metrics"
	"github.com/google/uuid"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

var errAssignmentAlreadyExists = errors.New("assignment already exists")

// Client handles Azure AD operations
type Client struct {
	graphClient       *msgraphsdk.GraphServiceClient
	servicePrincipals []string
	appRoleID         string // The app role ID to assign groups to
	mu                sync.RWMutex
	groupCache        map[string]models.Groupable // Cache groups by object ID
}

// NewClient creates a new Azure AD client using DefaultAzureCredential
func NewClient(ctx context.Context) (*Client, error) {
	// Get service principals from environment variable
	spEnv := os.Getenv("AZURE_SERVICE_PRINCIPALS")
	if spEnv == "" {
		return nil, fmt.Errorf("AZURE_SERVICE_PRINCIPALS environment variable not set")
	}
	servicePrincipals := strings.Split(spEnv, ",")
	for i, sp := range servicePrincipals {
		servicePrincipals[i] = strings.TrimSpace(sp)
	}

	// Get app role ID from environment variable (required)
	appRoleID := os.Getenv("AZURE_APP_ROLE_ID")
	if appRoleID == "" {
		return nil, fmt.Errorf("AZURE_APP_ROLE_ID environment variable not set")
	}

	return NewClientForTarget(ctx, servicePrincipals, appRoleID)
}

// NewClientForTarget creates a new Azure AD client for a specific assignment target.
func NewClientForTarget(ctx context.Context, servicePrincipals []string, appRoleID string) (*Client, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		appmetrics.AuthFailuresTotal.WithLabelValues("credential_init").Inc()
		return nil, fmt.Errorf("failed to create credential: %w", err)
	}

	// Create Graph client
	graphClient, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, []string{"https://graph.microsoft.com/.default"})
	if err != nil {
		appmetrics.AuthFailuresTotal.WithLabelValues("credential_init").Inc()
		return nil, fmt.Errorf("failed to create graph client: %w", err)
	}

	return &Client{
		graphClient:       graphClient,
		servicePrincipals: servicePrincipals,
		appRoleID:         appRoleID,
		groupCache:        make(map[string]models.Groupable),
	}, nil
}

// GetGroupByID retrieves a group by its Azure object ID.
func (c *Client) GetGroupByID(ctx context.Context, groupID string) (models.Groupable, error) {
	c.mu.RLock()
	if group, ok := c.groupCache[groupID]; ok {
		c.mu.RUnlock()
		appmetrics.GroupCacheRequestsTotal.WithLabelValues("hit").Inc()
		return group, nil
	}
	c.mu.RUnlock()
	appmetrics.GroupCacheRequestsTotal.WithLabelValues("miss").Inc()

	if _, err := uuid.Parse(groupID); err != nil {
		return nil, fmt.Errorf("invalid group object ID %q: %w", groupID, err)
	}

	start := time.Now()
	group, err := c.graphClient.Groups().ByGroupId(groupID).Get(ctx, nil)
	appmetrics.ObserveAzureRequest("get_group", start, err)
	if err != nil {
		return nil, fmt.Errorf("failed to get group by object ID %s: %w", groupID, err)
	}

	// Cache the group
	c.mu.Lock()
	c.groupCache[groupID] = group
	appmetrics.GroupCacheEntries.Set(float64(len(c.groupCache)))
	c.mu.Unlock()

	return group, nil
}

// AssignGroupToServicePrincipals assigns a group to all configured service principals
func (c *Client) AssignGroupToServicePrincipals(ctx context.Context, groupID string) error {
	var errs []error

	for _, spID := range c.servicePrincipals {
		if err := c.assignGroupToServicePrincipal(ctx, spID, groupID); err != nil {
			if errors.Is(err, errAssignmentAlreadyExists) {
				appmetrics.AssignmentOperationsTotal.WithLabelValues("assign", "already_exists").Inc()
				continue
			}
			appmetrics.AssignmentOperationsTotal.WithLabelValues("assign", appmetrics.ClassifyAzureError(err)).Inc()
			errs = append(errs, fmt.Errorf("failed to assign group %s to SP %s: %w", groupID, spID, err))
		} else {
			appmetrics.AssignmentOperationsTotal.WithLabelValues("assign", "success").Inc()
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors assigning group: %v", errs)
	}

	return nil
}

// RemoveGroupFromServicePrincipals removes a group assignment from all configured service principals
func (c *Client) RemoveGroupFromServicePrincipals(ctx context.Context, groupID string) error {
	var errs []error

	for _, spID := range c.servicePrincipals {
		if err := c.removeGroupFromServicePrincipal(ctx, spID, groupID); err != nil {
			appmetrics.AssignmentOperationsTotal.WithLabelValues("remove", appmetrics.ClassifyAzureError(err)).Inc()
			errs = append(errs, fmt.Errorf("failed to remove group %s from SP %s: %w", groupID, spID, err))
		} else {
			appmetrics.AssignmentOperationsTotal.WithLabelValues("remove", "success").Inc()
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors removing group: %v", errs)
	}

	return nil
}

// removeGroupFromServicePrincipal removes a group assignment from a service principal
func (c *Client) removeGroupFromServicePrincipal(ctx context.Context, spID, groupID string) error {
	// Get all assignments to find the one we need to delete
	start := time.Now()
	result, err := c.graphClient.ServicePrincipals().ByServicePrincipalId(spID).AppRoleAssignedTo().Get(ctx, nil)
	appmetrics.ObserveAzureRequest("list_assignments", start, err)
	if err != nil {
		return fmt.Errorf("failed to list app role assignments: %w", err)
	}

	// Find the assignment for this group
	assignments := result.GetValue()
	var assignmentID *string
	for _, assignment := range assignments {
		principalID := assignment.GetPrincipalId()
		if principalID != nil && principalID.String() == groupID {
			assignmentID = assignment.GetId()
			break
		}
	}

	if assignmentID == nil {
		// Assignment doesn't exist, nothing to do
		return nil
	}

	// Delete the assignment
	start = time.Now()
	err = c.graphClient.ServicePrincipals().ByServicePrincipalId(spID).AppRoleAssignedTo().ByAppRoleAssignmentId(*assignmentID).Delete(ctx, nil)
	appmetrics.ObserveAzureRequest("delete_assignment", start, err)
	if err != nil {
		return fmt.Errorf("failed to delete app role assignment: %w", err)
	}

	return nil
}

// assignGroupToServicePrincipal assigns a single group to a service principal
// Note: The controller's app registration must be an owner of the target service principal's
// app registration for this to work with Application.ReadWrite.OwnedBy permission
func (c *Client) assignGroupToServicePrincipal(ctx context.Context, spID, groupID string) error {
	// Check if assignment already exists
	exists, err := c.isGroupAssigned(ctx, spID, groupID)
	if err != nil {
		return fmt.Errorf("failed to check existing assignment: %w", err)
	}
	if exists {
		return errAssignmentAlreadyExists
	}

	// Create app role assignment
	// This creates a group membership assignment to the service principal
	requestBody := models.NewAppRoleAssignment()

	principalUUID, err := uuid.Parse(groupID)
	if err != nil {
		return fmt.Errorf("failed to parse group ID as UUID: %w", err)
	}
	requestBody.SetPrincipalId(&principalUUID)

	resourceUUID, err := uuid.Parse(spID)
	if err != nil {
		return fmt.Errorf("failed to parse service principal ID as UUID: %w", err)
	}
	requestBody.SetResourceId(&resourceUUID)

	// Use configured app role ID
	appRoleUUID, err := uuid.Parse(c.appRoleID)
	if err != nil {
		return fmt.Errorf("failed to parse app role ID as UUID: %w", err)
	}
	requestBody.SetAppRoleId(&appRoleUUID)

	start := time.Now()
	_, err = c.graphClient.ServicePrincipals().ByServicePrincipalId(spID).AppRoleAssignedTo().Post(ctx, requestBody, nil)
	appmetrics.ObserveAzureRequest("create_assignment", start, err)
	if err != nil {
		// Treat duplicate assignment responses as success for idempotency.
		if isAlreadyAssignedError(err) {
			return errAssignmentAlreadyExists
		}
		return fmt.Errorf("failed to create app role assignment: %w", err)
	}

	return nil
}

// isGroupAssigned checks if a group is already assigned to a service principal
func (c *Client) isGroupAssigned(ctx context.Context, spID, groupID string) (bool, error) {
	// Get all assignments (filtering is not supported on this collection)
	start := time.Now()
	result, err := c.graphClient.ServicePrincipals().ByServicePrincipalId(spID).AppRoleAssignedTo().Get(ctx, nil)
	appmetrics.ObserveAzureRequest("list_assignments", start, err)
	if err != nil {
		return false, err
	}

	// Check if any assignment matches our groupID
	assignments := result.GetValue()
	for _, assignment := range assignments {
		principalID := assignment.GetPrincipalId()
		if principalID != nil && principalID.String() == groupID {
			return true, nil
		}
	}

	return false, nil
}

// ListManagedAssignedGroupIDs returns the union of group principal IDs that are
// currently assigned to any of the configured service principals via this
// controller's app role.
//
// Only assignments matching the configured appRoleID are returned. This scopes
// the result to assignments this controller owns and avoids reporting (and
// later removing) assignments created by other mechanisms or with other roles.
// This set is treated as the "actual" state during full-state reconciliation.
func (c *Client) ListManagedAssignedGroupIDs(ctx context.Context) (map[string]struct{}, error) {
	appRoleUUID, err := uuid.Parse(c.appRoleID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app role ID as UUID: %w", err)
	}

	assigned := make(map[string]struct{})
	for _, spID := range c.servicePrincipals {
		start := time.Now()
		result, err := c.graphClient.ServicePrincipals().ByServicePrincipalId(spID).AppRoleAssignedTo().Get(ctx, nil)
		appmetrics.ObserveAzureRequest("list_assignments", start, err)
		if err != nil {
			return nil, fmt.Errorf("failed to list app role assignments for SP %s: %w", spID, err)
		}

		for _, assignment := range result.GetValue() {
			// Only consider assignments created via this controller's app role.
			assignmentAppRole := assignment.GetAppRoleId()
			if assignmentAppRole == nil || *assignmentAppRole != appRoleUUID {
				continue
			}

			principalID := assignment.GetPrincipalId()
			if principalID == nil {
				continue
			}
			assigned[principalID.String()] = struct{}{}
		}
	}

	return assigned, nil
}

// ClearCache clears the group cache
func (c *Client) ClearCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groupCache = make(map[string]models.Groupable)
	appmetrics.GroupCacheEntries.Set(0)
}

func isAlreadyAssignedError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "permission being assigned already exists") ||
		strings.Contains(errMsg, "entitlementgrant entry already exists")
}
