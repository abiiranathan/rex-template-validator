// Package handlers
package handlers

import (
	"errors"
	"fmt"
	"html/template"

	"github.com/abiiranathan/rex"
)

// Breadcrumb represents a navigation breadcrumb
type Breadcrumb struct {
	Label  string
	URL    string
	IsLast bool
}

// Breadcrumbs is a slice of Breadcrumb
type Breadcrumbs []Breadcrumb

// Visit represents a patient visit
type Visit struct {
	ID        uint
	PatientID uint
	Patient   Patient
	Doctor    Doctor
}

// Patient represents a patient
type Patient struct {
	Name string // Patient Full name
	ID   uint   // Patient ID
}

// Doctor represents a doctor
type Doctor struct {
	DisplayName string
	ID          uint
}

// Drug represents a drug
type Drug struct {
	Name     string // Drug Name
	Quantity int
	Price    float64
}

// User represents a user
type User struct {
	Name string
}

// PrintName prints the name of the user.
func (u *User) PrintName(name string) {
	fmt.Println(name)
}

// Prescription represents a prescription
type Prescription struct {
	// Drug name
	DrugName string

	// Quantity
	Quantity int
	Dosage   string // Dosage
	Drug     Drug   // The drug object
}

// Management represents a management entry
type Management struct {
	Prescription Prescription
}

// Handler holds service dependencies
type Handler struct{}

// getAuthUser returns the logged in user.
func getAuthUser(userID int) *User {
	fmt.Println(userID)
	return &User{}
}

// dict function that creates a map from a list of key-value pairs.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, errors.New("invalid dict call")
	}

	d := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, errors.New("dict keys must be strings")
		}
		d[key] = values[i+1]
	}
	return d, nil
}

// RenderTreatmentChart renders the treatment chart
func (h *Handler) RenderTreatmentChart(inpatient bool) rex.HandlerFunc {
	return func(c *rex.Context) error {
		visitID := c.ParamUint("visit_id")
		visit := &Visit{ID: visitID}

		var billedDrugs []Drug
		var prescriptions []Prescription
		var management []Management

		pathPrefix := "/inpatient"
		title := "Inpatient Treatment Chart"
		label := "Inpatient"

		newuser := &User{}

		if !inpatient {
			pathPrefix = "/outpatient"
			title = "OPD Progressive Treatment Chart"
			label = "OPD"
		}

		// Func Map
		funcMap := template.FuncMap{
			"getAuthUser": getAuthUser,
			"dict":        dict,
		}

		template.New("").Funcs(funcMap)

		// Analyzer also detects calls to c.Set and updated the index.
		c.Set("currentUser", newuser)

		// Magic happens here. Try renaming template name to something not found!
		return c.Render("views/inpatient/treatment-chart.html", rex.Map{
			"management":    management,
			"visit":         visit,
			"Title":         title,
			"newuser":       newuser,
			"PathPrefix":    pathPrefix,
			"billedDrugs":   billedDrugs,
			"prescriptions": prescriptions,
			"doctor":        visit.Doctor.DisplayName,
			"roles":         map[string]string{"admin": "Administrator", "user": "Normal User"},
			"breadcrumbs": Breadcrumbs{
				{Label: label, URL: pathPrefix},
				{Label: visit.Patient.Name, URL: fmt.Sprintf("/patients/%d", visit.PatientID)},
				{Label: "Treatment Chart", IsLast: true},
			},
		})
	}
}

func (h *Handler) RenderDashboard(inpatient bool) rex.HandlerFunc {
	return func(c *rex.Context) error {
		visitID := c.ParamUint("visit_id")
		templateName := "views/dashboard.html" // Dynamic template

		// Magic happens here. Try renaming template name to something not found!
		return c.Render(templateName, rex.Map{
			"visitID": visitID,
		})
	}
}
