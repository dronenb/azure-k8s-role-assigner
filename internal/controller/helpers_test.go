package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestIsValidAzureUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "valid lowercase uuid", in: "11111111-1111-1111-1111-111111111111", want: true},
		{name: "valid mixed-case uuid is normalized", in: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", want: true},
		{name: "valid realistic uuid", in: "8f4c2b1a-3d5e-4f6a-9b8c-1d2e3f4a5b6c", want: true},
		{name: "empty string", in: "", want: false},
		{name: "not a uuid", in: "platform-admins", want: false},
		{name: "missing hyphens", in: "111111111111111111111111111111111111", want: false},
		{name: "too short", in: "1111-1111", want: false},
		{name: "extra characters", in: "11111111-1111-1111-1111-111111111111-extra", want: false},
		{name: "non-hex characters", in: "zzzzzzzz-1111-1111-1111-111111111111", want: false},
		{name: "system group name", in: "system:masters", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isValidAzureUUID(tc.in))
		})
	}
}

func TestExtractGroupsFromSubjects(t *testing.T) {
	const (
		groupA = "11111111-1111-1111-1111-111111111111"
		groupB = "22222222-2222-2222-2222-222222222222"
	)

	tests := []struct {
		name     string
		subjects []rbacv1.Subject
		want     []string
	}{
		{
			name:     "nil subjects",
			subjects: nil,
			want:     []string{},
		},
		{
			name: "single valid group",
			subjects: []rbacv1.Subject{
				{Kind: "Group", Name: groupA},
			},
			want: []string{groupA},
		},
		{
			name: "non-group kinds are ignored",
			subjects: []rbacv1.Subject{
				{Kind: "User", Name: groupA},
				{Kind: "ServiceAccount", Name: groupB},
			},
			want: []string{},
		},
		{
			name: "system and kubeadm groups are ignored",
			subjects: []rbacv1.Subject{
				{Kind: "Group", Name: "system:masters"},
				{Kind: "Group", Name: "system:authenticated"},
				{Kind: "Group", Name: "kubeadm:cluster-admins"},
			},
			want: []string{},
		},
		{
			name: "non-uuid group names are ignored",
			subjects: []rbacv1.Subject{
				{Kind: "Group", Name: "platform-admins"},
				{Kind: "Group", Name: "my-org:team-x"},
			},
			want: []string{},
		},
		{
			name: "duplicates within a binding are de-duplicated",
			subjects: []rbacv1.Subject{
				{Kind: "Group", Name: groupA},
				{Kind: "Group", Name: groupA},
			},
			want: []string{groupA},
		},
		{
			name: "mixed valid and invalid keeps only valid uuids, in order",
			subjects: []rbacv1.Subject{
				{Kind: "User", Name: "alice"},
				{Kind: "Group", Name: "system:masters"},
				{Kind: "Group", Name: groupA},
				{Kind: "Group", Name: "not-a-uuid"},
				{Kind: "Group", Name: groupB},
				{Kind: "Group", Name: groupA},
			},
			want: []string{groupA, groupB},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractGroupsFromSubjects(tc.subjects, "test")
			assert.Equal(t, tc.want, got)
		})
	}
}
