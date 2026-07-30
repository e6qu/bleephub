package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnterpriseRepositoryPropertySchemaLifecycleAndPromotion(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterprisePropertyRoutes()
	base := "/api/v3/enterprises/bleephub/properties/schema"

	rec := enterpriseActionsRequest(t, s, http.MethodPut, base+"/environment", map[string]interface{}{
		"value_type": "single_select", "required": true, "default_value": "production",
		"allowed_values": []string{"production", "development"},
		"description":    "Deployment environment",
	})
	property := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || property["property_name"] != "environment" ||
		property["source_type"] != "enterprise" || property["values_editable_by"] != "org_actors" {
		t.Fatalf("create enterprise repository property = %d %#v", rec.Code, property)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base, nil)
	var listed []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list enterprise repository properties = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "property-promotion-org", "Property Promotion", "")
	description := "Owned by the organization"
	s.store.UpsertCustomProperty(org.Login, &CustomProperty{
		PropertyName: "service", ValueType: "string", Description: &description, ValuesEditableBy: "org_actors",
	})
	rec = enterpriseActionsRequest(t, s, http.MethodPut,
		base+"/organizations/"+org.Login+"/service/promote", nil)
	promoted := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || promoted["property_name"] != "service" ||
		promoted["source_type"] != "enterprise" {
		t.Fatalf("promote organization property = %d %#v", rec.Code, promoted)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, base+"/environment", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete enterprise repository property = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/environment", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted enterprise repository property = %d %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestEnterpriseOrganizationPropertyDefinitionsAndValues(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterprisePropertyRoutes()
	admin := s.store.LookupUserByLogin("admin")
	orgA := s.store.CreateOrg(admin, "enterprise-property-a", "Property A", "")
	orgB := s.store.CreateOrg(admin, "enterprise-property-b", "Property B", "")
	base := "/api/v3/enterprises/bleephub/org-properties"

	rec := enterpriseActionsRequest(t, s, http.MethodPatch, base+"/schema", map[string]interface{}{
		"properties": []map[string]interface{}{{
			"property_name": "cost_center", "value_type": "string",
			"values_editable_by": "enterprise_and_org_actors",
		}, {
			"property_name": "regulated", "value_type": "true_false", "default_value": "false",
		}},
	})
	var definitions []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &definitions); err != nil {
		t.Fatalf("decode definitions %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || len(definitions) != 2 ||
		definitions[0]["source_type"] != "enterprise" {
		t.Fatalf("enterprise organization property definitions = %d %#v", rec.Code, definitions)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/values", map[string]interface{}{
		"organization_logins": []string{orgA.Login, orgB.Login},
		"properties": []map[string]interface{}{{
			"property_name": "cost_center", "value": "CC-101",
		}, {
			"property_name": "regulated", "value": "true",
		}},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set enterprise organization values = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, base+"/values", nil)
	var values []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &values); err != nil {
		t.Fatalf("decode organization values: %v", err)
	}
	if rec.Code != http.StatusOK || len(values) != 2 {
		t.Fatalf("enterprise organization property values = %d %#v", rec.Code, values)
	}
	for _, row := range values {
		properties, _ := row["properties"].([]interface{})
		if len(properties) != 2 {
			t.Fatalf("organization value row = %#v", row)
		}
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, base+"/values", map[string]interface{}{
		"organization_logins": []string{"missing"},
		"properties":          []map[string]interface{}{{"property_name": "cost_center", "value": "CC-102"}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("set values for missing org = %d %q, want 422", rec.Code, rec.Body.String())
	}
}
