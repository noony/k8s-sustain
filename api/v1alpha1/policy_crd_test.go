package v1alpha1_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func policyCRD(t *testing.T) *apiextensions.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile("../../charts/k8s-sustain/files/crds/k8s.sustain.io_policies.yaml")
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var v1 apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &v1); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1, &crd, nil); err != nil {
		t.Fatalf("convert CRD: %v", err)
	}
	return &crd
}

// The apiserver would accept the generated CRD: its schema is structural and
// every CEL rule compiles within its cost budget.
func TestPolicyCRDPassesTheAPIServersValidation(t *testing.T) {
	crd := policyCRD(t)
	crd.Status.StoredVersions = []string{crd.Spec.Versions[0].Name}
	if errs := validation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) > 0 {
		t.Fatal(errs.ToAggregate())
	}
}

// controllerAddedLabels are the labels a controller writes on pods, or on a
// Job's pod template, that a selector may not use: the controller matches pod
// templates and the webhook pods, and the two disagree on these.
var controllerAddedLabels = []string{
	"pod-template-hash",
	"rollouts-pod-template-hash",
	"controller-revision-hash",
	"pod-template-generation",
	"statefulset.kubernetes.io/pod-name",
	"apps.kubernetes.io/pod-index",
	"controller-uid",
	"job-name",
	"batch.kubernetes.io/controller-uid",
	"batch.kubernetes.io/job-name",
	"batch.kubernetes.io/job-completion-index",
}

func validatePolicy(t *testing.T, crd *apiextensions.CustomResourceDefinition, policy string) field.ErrorList {
	t.Helper()
	// The conversion hoists the schema of a single version to spec.validation.
	s, err := structuralschema.NewStructural(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatalf("structural schema: %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(policy), &obj); err != nil {
		t.Fatalf("parse Policy: %v", err)
	}
	errs, _ := cel.NewValidator(s, true, celconfig.PerCallLimit).
		Validate(context.Background(), field.NewPath(""), s, obj, nil, celconfig.RuntimeCELCostBudget)
	return errs
}

func TestPolicyLabelSelectorRejectsControllerAddedLabels(t *testing.T) {
	crd := policyCRD(t)
	selector := func(s string) string {
		return "apiVersion: k8s.sustain.io/v1alpha1\nkind: Policy\nmetadata: {name: p}\nspec: {selector: {labelSelector: " + s + "}}"
	}
	for _, key := range controllerAddedLabels {
		for form, s := range map[string]string{
			"matchLabels":      `{matchLabels: {tier: web, "` + key + `": x}}`,
			"matchExpressions": `{matchExpressions: [{key: tier, operator: Exists}, {key: "` + key + `", operator: DoesNotExist}]}`,
		} {
			t.Run(form+"/"+key, func(t *testing.T) {
				errs := validatePolicy(t, crd, selector(s))
				if len(errs) != 1 || !strings.Contains(errs[0].Error(), form+" may not use a label") {
					t.Errorf("errors = %v, want %s refused", errs, form)
				}
			})
		}
	}
	for name, s := range map[string]string{
		"empty":                      `{}`,
		"user labels":                `{matchLabels: {tier: web, app: api}, matchExpressions: [{key: track, operator: NotIn, values: [canary]}]}`,
		"a prefix of a reserved key": `{matchLabels: {"job": x}, matchExpressions: [{key: "batch.kubernetes.io/job", operator: Exists}]}`,
		"k8s-sustain's own keys":     `{matchLabels: {"k8s.sustain.io/owner-name": x}, matchExpressions: [{key: "k8s.sustain.io/policy", operator: Exists}]}`,
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			if errs := validatePolicy(t, crd, selector(s)); len(errs) > 0 {
				t.Errorf("errors = %v, want none", errs)
			}
		})
	}
}
