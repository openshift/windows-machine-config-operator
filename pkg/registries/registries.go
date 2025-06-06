package registries

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	config "github.com/openshift/api/config/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// imagePathSeparator separates the repo name, namespaces, and image name in an OCI-compliant image name
const imagePathSeparator = "/"

var (
	GlobalPullSecretNamespace = "openshift-config"
	GlobalPullSecretName      = "pull-secret"
)

// mirror represents a mirrored image repo entry in a registry configuration file
type mirror struct {
	// host is the mirror image location. Can include the registry hostname/IP address, port, and namespace path
	host string
	// resolveTags indicates to the container runtime if this mirror is allowed to resolve an image tag into a digest
	resolveTags bool
}

// newMirror constructs a new mirror object with proper host name structure to be used in containerd registry config
func newMirror(sourceImageLocation, mirrorImageLocation string, resolveTags bool) mirror {
	mirrorHost := ""
	// containerd appends any shared namespaces between source and mirror locations to the mirror's host entry in the
	// registry config file to construct the full mirror image location
	if sourceImageLocation != mirrorImageLocation {
		// truncate the mirror to drop any shared namespaces since containerd automatically appends them on image pull
		mirrorHost = extractMirrorURL(sourceImageLocation, mirrorImageLocation)
	} else {
		// special case if source and mirror are the same. Do not drop the host repo name to avoid an empty host entry
		mirrorHost = extractRegistryHostname(mirrorImageLocation)
	}
	return mirror{host: mirrorHost, resolveTags: resolveTags}
}

// extractMirrorURL drops the common suffix from the second repo, returning only the unique leading URL and namespaces
func extractMirrorURL(source, mirror string) string {
	sourceParts := strings.Split(source, imagePathSeparator)
	mirrorParts := strings.Split(mirror, imagePathSeparator)
	uniqueMirrorParts := mirrorParts

	// Process until the end of either repo string
	for i := 0; i < len(sourceParts) && i < len(mirrorParts); i++ {
		// Check if suffix piece is equal, starting from the backs of the lists
		if sourceParts[len(sourceParts)-1-i] != mirrorParts[len(mirrorParts)-1-i] {
			// break when something different is found to retain all pieces after the last common element
			break
		}
		// Remove common suffix piece
		uniqueMirrorParts = uniqueMirrorParts[:len(uniqueMirrorParts)-1]
	}
	return strings.Join(uniqueMirrorParts, imagePathSeparator)
}

// mirrorSet holds the mirror registry information for a single source image repo
type mirrorSet struct {
	// source is the image repo to be mirrored
	source string
	// mirrors represents mirrored repository locations to pull images from rather than the default source
	mirrors []mirror
	// mirrorSourcePolicy defines the fallback policy if fails to pull image from the mirrors
	mirrorSourcePolicy config.MirrorSourcePolicy
}

// newMirrorSet constructs an object with proper source and mirror name structures to be used in containerd registry config
func newMirrorSet(srcImage string, mirrorLocations []config.ImageMirror, resolveTags bool,
	mirrorSourcePolicy config.MirrorSourcePolicy) mirrorSet {
	truncatedMirrors := []mirror{}
	for _, m := range mirrorLocations {
		truncatedMirrors = append(truncatedMirrors, newMirror(srcImage, string(m), resolveTags))
	}
	return mirrorSet{source: extractRegistryHostname(srcImage), mirrors: truncatedMirrors, mirrorSourcePolicy: mirrorSourcePolicy}
}

// extractRegistryHostname extracts just the initial host repo from a full image location, as containerd does not allow
// registries to exist on a subpath, given an input image `mcr.microsoft.com/oss/kubernetes/pause:3.9`,
// mcr.microsoft.com would be the determined registry hostname.
func extractRegistryHostname(fullImage string) string {
	// url.Parse will only work if URL has a scheme (https://)
	if parsedURL, err := url.Parse(fullImage); err == nil && parsedURL.Hostname() != "" {
		if parsedURL.Port() != "" {
			return parsedURL.Hostname() + ":" + parsedURL.Port()
		}
		return parsedURL.Hostname()
	}
	// For URLs without a scheme, just return everything before the first `/`
	return strings.Split(fullImage, imagePathSeparator)[0]
}

// extractRegistryOrgPath returns only the org path when given a reference to a registry.
// The input for this function should not include an image name.
func extractRegistryOrgPath(registry string) string {
	hostname := extractRegistryHostname(registry)
	hostnameSplit := strings.SplitN(registry, hostname, 2)
	if len(hostnameSplit) != 2 {
		return ""
	}
	return strings.TrimPrefix(hostnameSplit[1], "/")
}

// getMergedMirrorSets extracts and merges the contents of the given mirror sets.
// The resulting slice of mirrorSets represents a system-wide image registry configuration.
func getMergedMirrorSets(idmsItems []config.ImageDigestMirrorSet, idtsItems []config.ImageTagMirrorSet) []mirrorSet {
	// Each member of the allMirrorSets collection represents the registry configuration for a specific source
	var allMirrorSets []mirrorSet

	for _, idms := range idmsItems {
		for _, entry := range idms.Spec.ImageDigestMirrors {
			set := newMirrorSet(entry.Source, entry.Mirrors, false, entry.MirrorSourcePolicy)
			allMirrorSets = append(allMirrorSets, set)
		}
	}
	for _, itms := range idtsItems {
		for _, entry := range itms.Spec.ImageTagMirrors {
			set := newMirrorSet(entry.Source, entry.Mirrors, true, entry.MirrorSourcePolicy)
			allMirrorSets = append(allMirrorSets, set)
		}
	}

	return mergeMirrorSets(allMirrorSets)
}

// mergeMirrorSets consolidates duplicate entries in the given slice (based on the source) since we do not want to
// generate multiple config files for the same source image repo. Output is sorted to ensure it is deterministic.
func mergeMirrorSets(baseMirrorSets []mirrorSet) []mirrorSet {
	if len(baseMirrorSets) == 0 {
		return []mirrorSet{}
	}

	// Map to keep track of unique mirrorSets by source
	uniqueMirrorSets := make(map[string]mirrorSet)

	for _, ms := range baseMirrorSets {
		if existingMS, ok := uniqueMirrorSets[ms.source]; ok {
			// If the source already exists, merge its mirrors slices
			existingMS.mirrors = mergeMirrors(existingMS.mirrors, ms.mirrors)
			// If the existing source's mirrorSourcePolicy conflicts, NeverContactSource is preferred
			if existingMS.mirrorSourcePolicy == config.AllowContactingSource {
				existingMS.mirrorSourcePolicy = ms.mirrorSourcePolicy
			}
			// Update the map entry
			uniqueMirrorSets[ms.source] = existingMS
		} else {
			// If it does not exist, add it to the map
			uniqueMirrorSets[ms.source] = ms
		}
	}

	// Convert the map back to a slice with no duplicates sources
	var result []mirrorSet
	for _, ms := range uniqueMirrorSets {
		result = append(result, ms)
	}

	sortMirrorSets(result)
	return result
}

// sortMirrorSets sorts the mirrorSets and each set of underlying mirrors aplhabetically. Modifies the parameter in place
func sortMirrorSets(mirrorSets []mirrorSet) {
	// Sort mirrors by host alphabetically within each mirrorSet
	for i := range mirrorSets {
		sort.Slice(mirrorSets[i].mirrors, func(j, k int) bool {
			return mirrorSets[i].mirrors[j].host < mirrorSets[i].mirrors[k].host
		})
	}
	// Sort mirrorSets by source alphabetically
	sort.Slice(mirrorSets, func(i, j int) bool {
		return mirrorSets[i].source < mirrorSets[j].source
	})
}

// mergeMirrors consolidates duplicate mirrors in the given slice (based on the host) since we do not want to
// generate multiple entries in a single config file for the same mirror repo
func mergeMirrors(existingMirrors, newMirrors []mirror) []mirror {
	// Map to keep track of unique mirrors by host
	uniqueMirrors := make(map[string]mirror)

	// Iterate over existing mirrors and add them to the map
	for _, m := range existingMirrors {
		uniqueMirrors[m.host] = m
	}
	// Iterate over new mirrors
	for _, m := range newMirrors {
		if existingM, ok := uniqueMirrors[m.host]; ok {
			// If the mirror already exists, check the resolveTags field. Resolving by tag is preferred over by digest.
			if !existingM.resolveTags && m.resolveTags {
				uniqueMirrors[m.host] = m
			}
		} else {
			// If the mirror does not exist, add it to the map
			uniqueMirrors[m.host] = m
		}
	}

	// Convert the map back to a slice with no duplicates mirrors
	var result []mirror
	for _, m := range uniqueMirrors {
		result = append(result, m)
	}
	return result
}

// generateConfig is a serialization method that generates a valid TOML representation from a mirrorSet object.
// Results in content usable as a containerd image registry configuration file. Returns empty string if no mirrors exist
func (ms *mirrorSet) generateConfig(secretsConfig credentialprovider.DockerConfigJSON) string {
	if len(ms.mirrors) == 0 {
		return ""
	}

	result := ""

	fallbackServer := ms.source
	if ms.mirrorSourcePolicy == config.NeverContactSource {
		// set the fallback server to the first mirror to ensure the source is never contacted, even if all mirrors fail
		fallbackServer = ms.mirrors[0].host
	}
	fallbackRegistry := extractRegistryHostname(fallbackServer)
	result += fmt.Sprintf("server = \"https://%s/v2", fallbackRegistry)
	if orgPath := extractRegistryOrgPath(fallbackServer); orgPath != "" {
		result += "/" + orgPath
	}
	result += "\"\n"
	result += "\noverride_path = true\n"

	// Each mirror should result in an entry followed by a set of settings for interacting with the mirror host
	for _, m := range ms.mirrors {
		hostRegistry := extractRegistryHostname(m.host)
		hostOrgPath := extractRegistryOrgPath(m.host)
		result += "\n"
		result += fmt.Sprintf("[host.\"https://%s/v2", hostRegistry)
		if hostOrgPath != "" {
			result += "/" + hostOrgPath
		}
		result += "\"]\n"

		// Specify the operations the registry host may perform. IDMS mirrors can only be pulled by directly by digest,
		// whereas ITMS mirrors have the additional resolve capability, which allows converting a tag name into a digest
		var hostCapabilities string
		if m.resolveTags {
			hostCapabilities = "  capabilities = [\"pull\", \"resolve\"]"
		} else {
			hostCapabilities = "  capabilities = [\"pull\"]"
		}
		result += hostCapabilities
		result += "\n"
		result += "  override_path = true\n"

		// Extract the mirror repo's authorization credentials, if one exists
		if entry, ok := secretsConfig.Auths[extractRegistryHostname(m.host)]; ok {
			credentials := entry.Username + ":" + entry.Password
			token := base64.StdEncoding.EncodeToString([]byte(credentials))

			// Add the access token as a request header
			result += fmt.Sprintf("  [host.\"https://%s/v2", hostRegistry)
			if hostOrgPath != "" {
				result += "/" + hostOrgPath
			}
			result += "\".header]\n"
			result += fmt.Sprintf("    authorization = \"Basic %s\"", token)
			result += "\n"
		}
	}

	return result
}

// GenerateConfigFiles uses cluster resources to generate the containerd mirror registry configuration files
func GenerateConfigFiles(ctx context.Context, c client.Client) (map[string][]byte, error) {
	// List IDMS/ITMS resources
	imageDigestMirrorSetList := &config.ImageDigestMirrorSetList{}
	if err := c.List(ctx, imageDigestMirrorSetList); err != nil {
		return nil, fmt.Errorf("error getting IDMS list: %w", err)
	}
	imageTagMirrorSetList := &config.ImageTagMirrorSetList{}
	if err := c.List(ctx, imageTagMirrorSetList); err != nil {
		return nil, fmt.Errorf("error getting ITMS list: %w", err)
	}

	registryConf := getMergedMirrorSets(imageDigestMirrorSetList.Items, imageTagMirrorSetList.Items)

	// Check for registry authorization credentials
	pullSecret := &core.Secret{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: GlobalPullSecretNamespace, Name: GlobalPullSecretName},
		pullSecret)
	if err != nil {
		return nil, fmt.Errorf("error getting pull secret: %w", err)
	}
	var conf credentialprovider.DockerConfigJSON
	err = json.Unmarshal(pullSecret.Data[core.DockerConfigJsonKey], &conf)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling to DockerConfigJSON: %w", err)
	}

	// configFiles is a map from file path on the Windows node to the file content
	configFiles := make(map[string][]byte)
	for _, ms := range registryConf {
		// fileShortPath is the file path within containerd's config directory
		fileShortPath := fmt.Sprintf("%s\\hosts.toml", ms.source)
		configFiles[fileShortPath] = []byte(ms.generateConfig(conf))
	}
	return configFiles, nil
}
