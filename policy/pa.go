package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"

	"github.com/letsencrypt/boulder/core"
	berrors "github.com/letsencrypt/boulder/errors"
	"github.com/letsencrypt/boulder/features"
	"github.com/letsencrypt/boulder/iana"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/reloader"
)

// AuthorityImpl enforces CA policy decisions.
type AuthorityImpl struct {
	log blog.Logger

	blacklist              map[string]bool
	exactBlacklist         map[string]bool
	wildcardExactBlacklist map[string]bool
	blacklistMu            sync.RWMutex

	enabledChallenges          map[string]bool
	enabledChallengesWhitelist map[string]map[int64]bool
	pseudoRNG                  *rand.Rand
	rngMu                      sync.Mutex
}

// New constructs a Policy Authority.
func New(challengeTypes map[string]bool) (*AuthorityImpl, error) {

	pa := AuthorityImpl{
		log:               blog.Get(),
		enabledChallenges: challengeTypes,
		// We don't need real randomness for this.
		pseudoRNG: rand.New(rand.NewSource(99)),
	}

	return &pa, nil
}

type blacklistJSON struct {
	Blacklist      []string
	ExactBlacklist []string
}

// SetHostnamePolicyFile will load the given policy file, returning error if it
// fails. It will also start a reloader in case the file changes.
func (pa *AuthorityImpl) SetHostnamePolicyFile(f string) error {
	_, err := reloader.New(f, pa.loadHostnamePolicy, pa.hostnamePolicyLoadError)
	return err
}

func (pa *AuthorityImpl) hostnamePolicyLoadError(err error) {
	pa.log.AuditErrf("error loading hostname policy: %s", err)
}

func (pa *AuthorityImpl) loadHostnamePolicy(b []byte) error {
	hash := sha256.Sum256(b)
	pa.log.Infof("loading hostname policy, sha256: %s", hex.EncodeToString(hash[:]))
	var bl blacklistJSON
	err := json.Unmarshal(b, &bl)
	if err != nil {
		return err
	}
	if len(bl.Blacklist) == 0 {
		return fmt.Errorf("No entries in blacklist.")
	}
	nameMap := make(map[string]bool)
	for _, v := range bl.Blacklist {
		nameMap[v] = true
	}
	exactNameMap := make(map[string]bool)
	wildcardNameMap := make(map[string]bool)
	for _, v := range bl.ExactBlacklist {
		exactNameMap[v] = true
		// Remove the leftmost label of the exact blacklist entry to make an exact
		// wildcard blacklist entry that will prevent issuing a wildcard that would
		// include the exact blacklist entry. e.g. if "highvalue.example.com" is on
		// the exact blacklist we want "example.com" to be on the
		// wildcardExactBlacklist so that "*.example.com" cannot be issued.
		//
		// First, split the domain into two parts: the first label and the rest of the domain.
		parts := strings.SplitN(v, ".", 2)
		// if there are less than 2 parts then this entry is malformed! There should
		// at least be a "something." and a TLD like "com"
		if len(parts) < 2 {
			return fmt.Errorf(
				"Malformed exact blacklist entry, only one label: %q", v)
		}
		// Add the second part, the domain minus the first label, to the
		// wildcardNameMap to block issuance for `*.`+parts[1]
		wildcardNameMap[parts[1]] = true
	}
	pa.blacklistMu.Lock()
	pa.blacklist = nameMap
	pa.exactBlacklist = exactNameMap
	pa.wildcardExactBlacklist = wildcardNameMap
	pa.blacklistMu.Unlock()
	return nil
}

// SetChallengesWhitelistFile will load the given whitelist file, returning error if it
// fails. It will also start a reloader in case the file changes.
func (pa *AuthorityImpl) SetChallengesWhitelistFile(f string) error {
	_, err := reloader.New(f, pa.loadChallengesWhitelist, pa.challengesWhitelistLoadError)
	return err
}

func (pa *AuthorityImpl) challengesWhitelistLoadError(err error) {
	pa.log.AuditErrf("error loading challenges whitelist: %s", err)
}

func (pa *AuthorityImpl) loadChallengesWhitelist(b []byte) error {
	hash := sha256.Sum256(b)
	pa.log.Infof("loading challenges whitelist, sha256: %s", hex.EncodeToString(hash[:]))
	var wl map[string][]int64
	err := json.Unmarshal(b, &wl)
	if err != nil {
		return err
	}

	chalWl := make(map[string]map[int64]bool)

	for k, v := range wl {
		chalWl[k] = make(map[int64]bool)
		for _, i := range v {
			chalWl[k][i] = true
		}
	}

	pa.blacklistMu.Lock()
	pa.enabledChallengesWhitelist = chalWl
	pa.blacklistMu.Unlock()

	return nil
}

const (
	maxLabels = 10

	// RFC 1034 says DNS labels have a max of 63 octets, and names have a max of 255
	// octets: https://tools.ietf.org/html/rfc1035#page-10. Since two of those octets
	// are taken up by the leading length byte and the trailing root period the actual
	// max length becomes 253.
	// TODO(#3237): Right now our schema for the authz table only allows 255 characters
	// for identifiers, including JSON wrapping, which takes up 25 characters. For
	// now, we only allow identifiers up to 230 characters in length. When we are
	// able to do a migration to update this table, we can allow DNS names up to
	// 253 characters in length.
	maxLabelLength         = 63
	maxDNSIdentifierLength = 230
)

var dnsLabelRegexp = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,62}$")
var punycodeRegexp = regexp.MustCompile("^xn--")
var idnReservedRegexp = regexp.MustCompile("^[a-z0-9]{2}--")

func isDNSCharacter(ch byte) bool {
	return ('a' <= ch && ch <= 'z') ||
		('A' <= ch && ch <= 'Z') ||
		('0' <= ch && ch <= '9') ||
		ch == '.' || ch == '-'
}

var (
	errInvalidIdentifier    = berrors.MalformedError("Invalid identifier type")
	errNonPublic            = berrors.MalformedError("Name does not end in a public suffix")
	errICANNTLD             = berrors.MalformedError("Name is an ICANN TLD")
	errBlacklisted          = berrors.RejectedIdentifierError("Policy forbids issuing for name")
	errInvalidDNSCharacter  = berrors.MalformedError("Invalid character in DNS name")
	errNameTooLong          = berrors.MalformedError("DNS name too long")
	errIPAddress            = berrors.MalformedError("Issuance for IP addresses not supported")
	errTooManyLabels        = berrors.MalformedError("DNS name has too many labels")
	errEmptyName            = berrors.MalformedError("DNS name was empty")
	errNameEndsInDot        = berrors.MalformedError("DNS name ends in a period")
	errTooFewLabels         = berrors.MalformedError("DNS name does not have enough labels")
	errLabelTooShort        = berrors.MalformedError("DNS label is too short")
	errLabelTooLong         = berrors.MalformedError("DNS label is too long")
	errMalformedIDN         = berrors.MalformedError("DNS label contains malformed punycode")
	errInvalidRLDH          = berrors.RejectedIdentifierError("DNS name contains a R-LDH label")
	errTooManyWildcards     = berrors.MalformedError("DNS name had more than one wildcard")
	errMalformedWildcard    = berrors.MalformedError("DNS name had a malformed wildcard label")
	errICANNTLDWildcard     = berrors.MalformedError("DNS name was a wildcard for an ICANN TLD")
	errWildcardNotSupported = berrors.MalformedError("Wildcard names not supported")
)

// WillingToIssue determines whether the CA is willing to issue for the provided
// identifier. It expects domains in id to be lowercase to prevent mismatched
// cases breaking queries.
//
// We place several criteria on identifiers we are willing to issue for:
//
//  * MUST self-identify as DNS identifiers
//  * MUST contain only bytes in the DNS hostname character set
//  * MUST NOT have more than maxLabels labels
//  * MUST follow the DNS hostname syntax rules in RFC 1035 and RFC 2181
//    In particular:
//    * MUST NOT contain underscores
//  * MUST NOT match the syntax of an IP address
//  * MUST end in a public suffix
//  * MUST have at least one label in addition to the public suffix
//  * MUST NOT be a label-wise suffix match for a name on the black list,
//    where comparison is case-independent (normalized to lower case)
//
// If WillingToIssue returns an error, it will be of type MalformedRequestError
// or RejectedIdentifierError
func (pa *AuthorityImpl) WillingToIssue(id core.AcmeIdentifier) error {
	if id.Type != core.IdentifierDNS {
		return errInvalidIdentifier
	}
	domain := id.Value

	if domain == "" {
		return errEmptyName
	}

	if strings.HasPrefix(domain, "*.") {
		return errWildcardNotSupported
	}

	for _, ch := range []byte(domain) {
		if !isDNSCharacter(ch) {
			return errInvalidDNSCharacter
		}
	}

	if len(domain) > maxDNSIdentifierLength {
		return errNameTooLong
	}

	if ip := net.ParseIP(domain); ip != nil {
		return errIPAddress
	}

	if strings.HasSuffix(domain, ".") {
		return errNameEndsInDot
	}

	labels := strings.Split(domain, ".")
	if len(labels) > maxLabels {
		return errTooManyLabels
	}
	if len(labels) < 2 {
		return errTooFewLabels
	}
	for _, label := range labels {
		if len(label) < 1 {
			return errLabelTooShort
		}
		if len(label) > maxLabelLength {
			return errLabelTooLong
		}

		if !dnsLabelRegexp.MatchString(label) {
			return errInvalidDNSCharacter
		}

		if label[len(label)-1] == '-' {
			return errInvalidDNSCharacter
		}

		if punycodeRegexp.MatchString(label) {
			// We don't care about script usage, if a name is resolvable it was
			// registered with a higher power and they should be enforcing their
			// own policy. As long as it was properly encoded that is enough
			// for us.
			ulabel, err := idna.ToUnicode(label)
			if err != nil {
				return errMalformedIDN
			}
			if !norm.NFC.IsNormalString(ulabel) {
				return errMalformedIDN
			}
		} else if idnReservedRegexp.MatchString(label) {
			return errInvalidRLDH
		}
	}

	// Names must end in an ICANN TLD, but they must not be equal to an ICANN TLD.
	icannTLD, err := iana.ExtractSuffix(domain)
	if err != nil {
		return errNonPublic
	}
	if icannTLD == domain {
		return errICANNTLD
	}

	// Require no match against blacklist
	if err := pa.checkHostLists(domain); err != nil {
		return err
	}

	return nil
}

// WillingToIssueWildcard is an extension of WillingToIssue that accepts DNS
// identifiers for well formed wildcard domains. It enforces that:
// * The identifer is a DNS type identifier
// * There is at most one `*` wildcard character
// * That the wildcard character is the leftmost label
// * That the wildcard label is not immediately adjacent to a top level ICANN
//   TLD
// * That the wildcard wouldn't cover an exact blacklist entry (e.g. an exact
//   blacklist entry for "foo.example.com" should prevent issuance for
//   "*.example.com")
//
// If all of the above is true then the base domain (e.g. without the *.) is run
// through WillingToIssue to catch other illegal things (blocked hosts, etc).
func (pa *AuthorityImpl) WillingToIssueWildcard(ident core.AcmeIdentifier) error {
	// We're only willing to process DNS identifiers
	if ident.Type != core.IdentifierDNS {
		return errInvalidIdentifier
	}
	rawDomain := ident.Value

	// If there is more than one wildcard in the domain the ident is invalid
	if strings.Count(rawDomain, "*") > 1 {
		return errTooManyWildcards
	}

	// If there is exactly one wildcard in the domain we need to do some special
	// processing to ensure that it is a well formed wildcard request and to
	// translate the identifer to its base domain for use with WillingToIssue
	if strings.Count(rawDomain, "*") == 1 {
		// If the rawDomain has a wildcard character, but it isn't the first most
		// label of the domain name then the wildcard domain is malformed
		if !strings.HasPrefix(rawDomain, "*.") {
			return errMalformedWildcard
		}
		// The base domain is the wildcard request with the `*.` prefix removed
		baseDomain := strings.TrimPrefix(rawDomain, "*.")
		// Names must end in an ICANN TLD, but they must not be equal to an ICANN TLD.
		icannTLD, err := iana.ExtractSuffix(baseDomain)
		if err != nil {
			return errNonPublic
		}
		// Names must have a non-wildcard label immediately adjacent to the ICANN
		// TLD. No `*.com`!
		if baseDomain == icannTLD {
			return errICANNTLDWildcard
		}
		// The base domain can't be in the wildcard exact blacklist
		if err := pa.checkWildcardHostList(baseDomain); err != nil {
			return err
		}
		// Check that the PA is willing to issue for the base domain
		// Since the base domain without the "*." may trip the exact hostname policy
		// blacklist when the "*." is removed we replace it with a single "x"
		// character to differentiate "*.example.com" from "example.com" for the
		// exact hostname check.
		//
		// NOTE(@cpu): This is pretty hackish! Boulder issue #3323[0] describes
		// a better follow-up that we should land to replace this code.
		// [0] https://github.com/letsencrypt/boulder/issues/3323
		return pa.WillingToIssue(core.AcmeIdentifier{
			Type:  core.IdentifierDNS,
			Value: "x." + baseDomain,
		})
	}

	return pa.WillingToIssue(ident)
}

// checkWildcardHostList checks the wildcardExactBlacklist for a given domain.
// If the domain is not present on the list nil is returned, otherwise
// errBlacklisted is returned.
func (pa *AuthorityImpl) checkWildcardHostList(domain string) error {
	pa.blacklistMu.RLock()
	defer pa.blacklistMu.RUnlock()

	if pa.blacklist == nil {
		return fmt.Errorf("Hostname policy not yet loaded.")
	}

	if pa.wildcardExactBlacklist[domain] {
		return errBlacklisted
	}

	return nil
}

func (pa *AuthorityImpl) checkHostLists(domain string) error {
	pa.blacklistMu.RLock()
	defer pa.blacklistMu.RUnlock()

	if pa.blacklist == nil {
		return fmt.Errorf("Hostname policy not yet loaded.")
	}

	labels := strings.Split(domain, ".")
	for i := range labels {
		joined := strings.Join(labels[i:], ".")
		if pa.blacklist[joined] {
			return errBlacklisted
		}
	}

	if pa.exactBlacklist[domain] {
		return errBlacklisted
	}
	return nil
}

// ChallengesFor makes a decision of what challenges, and combinations, are
// acceptable for the given identifier. If the TLSSNIRevalidation feature flag
// is set, create TLS-SNI-01 challenges for revalidation requests even if
// TLS-SNI-01 is not among the configured challenges.
func (pa *AuthorityImpl) ChallengesFor(identifier core.AcmeIdentifier, regID int64, revalidation bool) ([]core.Challenge, [][]int, error) {
	challenges := []core.Challenge{}

	// If we are using the new authorization storage schema we only use a single
	// token for all challenges rather than a unique token per challenge.
	var token string
	if features.Enabled(features.NewAuthorizationSchema) {
		token = core.NewToken()
	}

	// If the identifier is for a DNS wildcard name we only
	// provide a DNS-01 challenge as a matter of CA policy.
	if strings.HasPrefix(identifier.Value, "*.") {
		// We must have the DNS-01 challenge type enabled to create challenges for
		// a wildcard identifier per LE policy.
		if !pa.ChallengeTypeEnabled(core.ChallengeTypeDNS01, regID) {
			return nil, nil, fmt.Errorf(
				"Challenges requested for wildcard identifier but DNS-01 " +
					"challenge type is not enabled")
		}
		// Only provide a DNS-01-Wildcard challenge
		challenges = []core.Challenge{core.DNSChallenge01(token)}
	} else {
		// Otherwise we collect up challenges based on what is enabled.
		if pa.ChallengeTypeEnabled(core.ChallengeTypeHTTP01, regID) {
			challenges = append(challenges, core.HTTPChallenge01(token))
		}

		// Add a TLS-SNI challenge, if either (a) the challenge is enabled, or (b)
		// the TLSSNIRevalidation feature flag is on and this is a revalidation.
		if pa.ChallengeTypeEnabled(core.ChallengeTypeTLSSNI01, regID) ||
			(features.Enabled(features.TLSSNIRevalidation) && revalidation) {
			challenges = append(challenges, core.TLSSNIChallenge01(token))
		}

		if pa.ChallengeTypeEnabled(core.ChallengeTypeTLSALPN01, regID) {
			challenges = append(challenges, core.TLSALPNChallenge01(token))
		}

		if pa.ChallengeTypeEnabled(core.ChallengeTypeDNS01, regID) {
			challenges = append(challenges, core.DNSChallenge01(token))
		}
	}

	// We shuffle the challenges and combinations to prevent ACME clients from
	// relying on the specific order that boulder returns them in.
	shuffled := make([]core.Challenge, len(challenges))
	combinations := make([][]int, len(challenges))

	pa.rngMu.Lock()
	defer pa.rngMu.Unlock()
	for i, challIdx := range pa.pseudoRNG.Perm(len(challenges)) {
		shuffled[i] = challenges[challIdx]
		combinations[i] = []int{i}
	}

	shuffledCombos := make([][]int, len(combinations))
	for i, comboIdx := range pa.pseudoRNG.Perm(len(combinations)) {
		shuffledCombos[i] = combinations[comboIdx]
	}

	return shuffled, shuffledCombos, nil
}

// ChallengeTypeEnabled returns whether the specified challenge type is enabled
func (pa *AuthorityImpl) ChallengeTypeEnabled(t string, regID int64) bool {
	pa.blacklistMu.RLock()
	defer pa.blacklistMu.RUnlock()
	return pa.enabledChallenges[t] ||
		(pa.enabledChallengesWhitelist[t] != nil && pa.enabledChallengesWhitelist[t][regID])
}
