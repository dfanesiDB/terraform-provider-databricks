package identity

import (
	"context"
	"fmt"
	"net/http"

	"github.com/databrickslabs/terraform-provider-databricks/common"
)

// NewGroupsAPI creates GroupsAPI instance from provider meta
func NewGroupsAPI(ctx context.Context, m interface{}) GroupsAPI {
	return GroupsAPI{
		client:  m.(*common.DatabricksClient),
		context: ctx,
	}
}

// GroupsAPI exposes the scim groups API
type GroupsAPI struct {
	client  *common.DatabricksClient
	context context.Context
}

// Create creates a scim group in the Databricks workspace
func (a GroupsAPI) Create(scimGroupRequest ScimGroup) (group ScimGroup, err error) {
	scimGroupRequest.Schemas = []URN{GroupSchema}
	err = a.client.Scim(a.context, http.MethodPost, "/preview/scim/v2/Groups", scimGroupRequest, &group)
	return
}

// Read reads and returns a Group object via SCIM api
func (a GroupsAPI) Read(groupID string) (group ScimGroup, err error) {
	err = a.client.Scim(a.context, http.MethodGet, fmt.Sprintf("/preview/scim/v2/Groups/%v", groupID), nil, &group)
	if err != nil {
		return
	}
	return
}

// Filter returns groups matching the filter
func (a GroupsAPI) Filter(filter string) (GroupList, error) {
	var groups GroupList
	req := map[string]string{}
	if filter != "" {
		req["filter"] = filter
	}
	err := a.client.Scim(a.context, http.MethodGet, "/preview/scim/v2/Groups", req, &groups)
	return groups, err
}

func (a GroupsAPI) ReadByDisplayName(displayName string) (group ScimGroup, err error) {
	groupList, err := a.Filter(fmt.Sprintf("displayName eq '%s'", displayName))
	if err != nil {
		return
	}
	if len(groupList.Resources) == 0 {
		err = fmt.Errorf("cannot find group: %s", displayName)
		return
	}
	group = groupList.Resources[0]
	return
}

func (a GroupsAPI) Patch(groupID string, r patchRequest) error {
	return a.client.Scim(a.context, http.MethodPatch, fmt.Sprintf("/preview/scim/v2/Groups/%v", groupID), r, nil)
}

func (a GroupsAPI) UpdateNameAndEntitlements(groupID string, name string, e entitlements) error {
	g, err := a.Read(groupID)
	if err != nil {
		return err
	}
	return a.client.Scim(a.context, http.MethodPut,
		fmt.Sprintf("/preview/scim/v2/Groups/%v", groupID),
		ScimGroup{
			DisplayName:  name,
			Entitlements: e,
			Groups:       g.Groups,
			Roles:        g.Roles,
			Members:      g.Members,
			Schemas:      []URN{GroupSchema},
		}, nil)
}

// Delete deletes a group given a group id
func (a GroupsAPI) Delete(groupID string) error {
	return a.client.Scim(a.context, http.MethodDelete,
		fmt.Sprintf("/preview/scim/v2/Groups/%v", groupID),
		nil, nil)
}
