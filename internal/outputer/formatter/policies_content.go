package formatter

import (
	"fmt"
	"strings"

	"github.com/Legit-Labs/legitify/internal/common/map_utils"
	"github.com/Legit-Labs/legitify/internal/enricher/enrichers"
	"github.com/Legit-Labs/legitify/internal/outputer/scheme"
	"github.com/iancoleman/orderedmap"
)

type policiesFormatter interface {
	FormatTitle(title string, severity string) string
	FormatSubtitle(title string) string
	FormatText(depth int, format string, args ...interface{}) string
	FormatList(depth int, title string, list []string, ordered bool, addListPrefix bool) string
	Linebreak() string
	Separator() string
	Indent(depth int) string
}

type policiesContent struct {
	pf        policiesFormatter
	colorizer colorizer
	sb        strings.Builder
	depth     int
}

func newPoliciesContent(pf policiesFormatter, colorizer colorizer) *policiesContent {
	return &policiesContent{
		pf:        pf,
		colorizer: colorizer,
	}
}

func (pc *policiesContent) FormatPolicy(output *scheme.Flattened, policyName string) []byte {
	if _, ok := output.AsOrderedMap().Get(policyName); !ok {
		return nil
	}

	pc.sb.Reset()

	pc.formatPolicyInfo(policyName, output, false)

	return []byte(pc.sb.String())
}

func (pc *policiesContent) FormatViolation(vioaltion *scheme.Violation) []byte {
	pc.sb.Reset()
	pc.writeViolation(vioaltion)
	return []byte(pc.sb.String())
}

func (pc *policiesContent) FormatFailedPolicies(output *scheme.Flattened) []byte {
	pc.sb.Reset()

	lastIndex := len(output.AsOrderedMap().Keys()) - 1
	for i, policyName := range output.AsOrderedMap().Keys() {
		pc.formatPolicyInfo(policyName, output, true)

		if i < lastIndex {
			pc.writeLineBreak()
		}
	}

	return []byte(pc.sb.String())
}

func (pc *policiesContent) formatPolicyInfo(policyName string, output *scheme.Flattened, withViolations bool) {
	policyData := output.GetPolicyData(policyName)

	pc.writeLine(pc.pf.FormatTitle(policyData.PolicyInfo.Title, policyData.PolicyInfo.Severity))

	pc.depth++
	pc.writePolicyInfo(policyName, policyData.PolicyInfo)
	pc.writeLineBreak()
	if withViolations {
		pc.writeViolations(policyData.Violations)
	}
	pc.depth--
}

func (pc *policiesContent) write(format string, args ...interface{}) {
	pc.sb.WriteString(pc.pf.FormatText(pc.depth, format, args...))
}

func (pc *policiesContent) writeLine(format string, args ...interface{}) {
	pc.write(format, args...)
	pc.write("%s", pc.pf.Linebreak())
}

func (pc *policiesContent) writeLineBreak() {
	pc.writeLine("")
}

func (pc *policiesContent) writeList(title string, list []string, ordered bool, addListPrefix bool) {
	title = fmt.Sprintf("%s:", pc.bold(title))
	pc.sb.WriteString(pc.pf.FormatList(pc.depth, title, list, ordered, addListPrefix))
}

func (pc *policiesContent) writeKeyval(key string, val string) {
	key = fmt.Sprintf("%s:", pc.bold(key))
	pc.sb.WriteString(pc.pf.FormatText(pc.depth, "%s %s", key, val) + pc.pf.Linebreak())
}

func (pc *policiesContent) writePolicyInfo(policyName string, policyInfo scheme.PolicyInfo) {
	pc.writeLine(pc.bold(policyInfo.Description))
	pc.writeLineBreak()

	pc.writeKeyval("Policy Name", policyInfo.PolicyName)
	pc.writeKeyval("Namespace", policyInfo.Namespace)
	coloredSeverity := pc.colorizer.colorize(severityToThemeColor(policyInfo.Severity), policyInfo.Severity)
	pc.writeKeyval("Severity", coloredSeverity)

	pc.writeLineBreak()
	pc.writeList("Threat", policyInfo.Threat, false, true)

	pc.writeLineBreak()
	pc.writeList("Remediation Steps", policyInfo.RemediationSteps, false, false)
}

func (pc *policiesContent) bold(text interface{}) string {
	return pc.colorizer.colorize(themeColorBold, text)
}

func (pc *policiesContent) writeViolations(violations []scheme.Violation) {
	pc.writeLine(pc.pf.FormatSubtitle("Violations:"))

	lastIndex := len(violations) - 1
	for i, violation := range violations {
		pc.writeViolation(&violation)
		if i < lastIndex {
			pc.writeLine(pc.pf.Separator())
		}
	}
}

func (pc *policiesContent) writeViolation(violation *scheme.Violation) {
	pc.writeKeyval(fmt.Sprintf("Link to %s", violation.ViolationEntityType), violation.CanonicalLink)
	pc.writeAux(violation.Aux)
}

func (pc *policiesContent) writeAux(aux *orderedmap.OrderedMap) {
	if aux == nil || len(aux.Keys()) == 0 {
		return
	}

	pc.writeList("Auxiliary Info", pc.auxAsList(aux), false, true)
}

func (pc *policiesContent) auxAsList(m *orderedmap.OrderedMap) []string {
	asList := make([]string, 0, len(m.Keys()))

	for _, k := range m.Keys() {
		v := map_utils.UnsafeGet[enrichers.Enrichment](m, k)
		key := camelCaseToTitle(k)
		prefix := pc.pf.Indent(pc.depth)
		vText := strings.TrimSuffix(v.HumanReadable(prefix, pc.pf.Linebreak()), pc.pf.Linebreak())
		formatted := fmt.Sprintf("%s: %v", pc.bold(key), vText)
		asList = append(asList, formatted)
	}

	return asList
}
