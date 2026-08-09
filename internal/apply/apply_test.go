package apply

import (
	"testing"
	"time"
)

func TestDetectATS(t *testing.T) {
	for _, c := range []struct {
		url, want string
		ok        bool
	}{
		{"https://boards.greenhouse.io/vercel/jobs/123", "Greenhouse", true},
		{"https://jobs.lever.co/anthropic/abc", "Lever", true},
		{"https://acme.myworkdayjobs.com/en-US/careers/job/123", "Workday", true},
		{"https://jobs.ashbyhq.com/openai/xyz", "Ashby", true},
		{"https://news.ycombinator.com", "", false},
	} {
		got, ok := DetectATS(c.url)
		if ok != c.ok || got != c.want {
			t.Errorf("DetectATS(%q) = %q,%v want %q,%v", c.url, got, ok, c.want, c.ok)
		}
	}
}

// The tracker must not invent applications. A newsletter that happens to
// mention jobs is the common false positive, and one bad row teaches you to
// distrust the whole tracker.
func TestFromEmailIgnoresNonConfirmations(t *testing.T) {
	for _, c := range []struct{ name, subject, from, body string }{
		{"newsletter", "This week in jobs", "digest@jobsweekly.com", "Hot roles at Vercel, Stripe and more. Apply now!"},
		{"rejection", "Update on your application", "no-reply@greenhouse.io", "We have decided to move forward with other candidates."},
		{"recruiter outreach", "Backend role at Acme", "jane@acme.com", "I saw your profile and thought you would be a great fit. Interested?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := FromEmail(c.subject, c.from, c.body, time.Now()); ok {
				t.Fatalf("wrongly parsed %q as an application confirmation", c.subject)
			}
		})
	}
}

func TestFromEmailExtractsRealConfirmation(t *testing.T) {
	app, ok := FromEmail(
		"Thank you for applying to Vercel",
		"Vercel Careers <no-reply@greenhouse.io>",
		"Thank you for applying to the Backend Engineer position at Vercel. We have received your application and will be in touch.",
		time.Now())
	if !ok {
		t.Fatal("did not recognise a real confirmation")
	}
	if app.Company != "Vercel" {
		t.Errorf("company = %q, want Vercel", app.Company)
	}
	if app.Role != "Backend Engineer" {
		t.Errorf("role = %q, want Backend Engineer", app.Role)
	}
	if app.ATS != "Greenhouse" {
		t.Errorf("ats = %q, want Greenhouse", app.ATS)
	}
}

// The display name is preferred over the sending domain, because ATS mail
// comes from the ATS. Treating greenhouse.io as the employer would file every
// application under one company.
func TestSenderParsingPrefersEmployerOverATS(t *testing.T) {
	if got := companyFromSender("Vercel Careers <no-reply@greenhouse.io>"); got != "Vercel" {
		t.Errorf("got %q, want Vercel with the Careers suffix stripped", got)
	}
	if got := companyFromSender("<careers@stripe.com>"); got != "Stripe" {
		t.Errorf("got %q, want Stripe from the domain", got)
	}
	if got := companyFromSender("Greenhouse <no-reply@greenhouse.io>"); got != "" {
		t.Errorf("got %q, want empty: the ATS is not the employer", got)
	}
}

// Reconciliation identity: the same application seen by the extension and then
// by email must collapse to one row, despite different URLs and timestamps.
func TestKeyReconcilesAcrossSources(t *testing.T) {
	fromExt := Application{
		Company: "Vercel", Role: "Backend Engineer",
		URL: "https://boards.greenhouse.io/vercel/jobs/123", Source: SourceExtension,
		AppliedAt: time.Now(),
	}
	fromEmail := Application{
		Company: "vercel", Role: "backend  engineer",
		Source: SourceEmail, AppliedAt: time.Now().Add(4 * time.Minute),
	}
	if fromExt.Key() != fromEmail.Key() {
		t.Fatalf("keys differ: %s vs %s; the same application would be tracked twice",
			fromExt.Key(), fromEmail.Key())
	}

	other := Application{Company: "Vercel", Role: "Frontend Engineer"}
	if other.Key() == fromExt.Key() {
		t.Fatal("different roles collapsed into one key")
	}
}
