package placement

import "kompensator/internal/repo"

// AppsForNode returns the apps that should run on a node holding the given
// roles. An app matches if it shares at least one role with the node. An app
// with no roles matches every node in the environment.
func AppsForNode(p repo.Placement, nodeRoles []string) []repo.App {
	roleSet := make(map[string]struct{}, len(nodeRoles))
	for _, r := range nodeRoles {
		roleSet[r] = struct{}{}
	}

	var matched []repo.App
	for _, app := range p.Apps {
		if len(app.Roles) == 0 || intersects(app.Roles, roleSet) {
			matched = append(matched, app)
		}
	}
	return matched
}

func intersects(roles []string, set map[string]struct{}) bool {
	for _, r := range roles {
		if _, ok := set[r]; ok {
			return true
		}
	}
	return false
}
