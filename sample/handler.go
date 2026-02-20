// Package handlers
package handlers

import (
	"fmt"

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
	Name     string
	Quantity int
	Price    float64
}

// User represents a user
type User struct {
	Name string
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

		currentUser := &User{}

		if !inpatient {
			pathPrefix = "/outpatient"
			title = "OPD Progressive Treatment Chart"
			label = "OPD"
		}

		return c.Render("views/inpatient/treatment-chart.html", rex.Map{
			"management":    management,
			"visit":         visit,
			"Title":         title,
			"newuser":       currentUser,
			"PathPrefix":    pathPrefix,
			"billedDrugs":   billedDrugs,
			"prescriptions": prescriptions,
			"doctor":        visit.Doctor.DisplayName,
			"breadcrumbs": Breadcrumbs{
				{Label: label, URL: pathPrefix},
				{Label: visit.Patient.Name, URL: fmt.Sprintf("/patients/%d", visit.PatientID)},
				{Label: "Treatment Chart", IsLast: true},
			},
		})
	}
}
