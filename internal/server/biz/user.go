package biz

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samber/lo"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/role"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/ent/userrole"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
)

var campusRegistrationEmailDomains = map[string]struct{}{
	"mails.ucas.ac.cn":  {},
	"ucas.ac.cn":        {},
	"mails.ucas.edu.cn": {},
	"ucas.edu.cn":       {},
}

var reservedCampusNicknames = map[string]struct{}{
	"owner": {}, "admin": {}, "administrator": {}, "system": {}, "official": {},
	"管理员": {}, "项目管理员": {}, "系统": {}, "官方": {}, "校方": {},
}

// NormalizeCampusNickname validates a voluntary public nickname. An empty
// nickname is valid and means the privacy-preserving stable alias is shown.
func NormalizeCampusNickname(nickname string) (string, error) {
	normalized := strings.TrimSpace(nickname)
	if normalized == "" {
		return "", nil
	}
	if !utf8.ValidString(normalized) {
		return "", fmt.Errorf("%w: nickname must be valid UTF-8", ErrInvalidNickname)
	}

	runeCount := utf8.RuneCountInString(normalized)
	if runeCount < 2 || runeCount > 24 {
		return "", fmt.Errorf("%w: use 2 to 24 characters", ErrInvalidNickname)
	}

	for _, r := range normalized {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("%w: control, bidi, and zero-width characters are not allowed", ErrInvalidNickname)
		}
	}

	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, normalized)
	if _, reserved := reservedCampusNicknames[compact]; reserved {
		return "", fmt.Errorf("%w: this name is reserved", ErrInvalidNickname)
	}
	if strings.HasPrefix(compact, "同学-") {
		return "", fmt.Errorf("%w: names beginning with the anonymous alias prefix are reserved", ErrInvalidNickname)
	}

	return normalized, nil
}

func normalizeCampusRegistrationEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(normalized, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || strings.ContainsAny(normalized, " \t\r\n") {
		return "", fmt.Errorf("%w; allowed domains: @mails.ucas.ac.cn, @ucas.ac.cn, @mails.ucas.edu.cn, @ucas.edu.cn", ErrCampusEmailRequired)
	}
	if _, ok := campusRegistrationEmailDomains[domain]; !ok {
		return "", fmt.Errorf("%w; allowed domains: @mails.ucas.ac.cn, @ucas.ac.cn, @mails.ucas.edu.cn, @ucas.edu.cn", ErrCampusEmailRequired)
	}

	return normalized, nil
}

func campusMemberProjectScopes() []string {
	return []string{
		string(scopes.ScopeReadAPIKeys),
		string(scopes.ScopeWriteAPIKeys),
	}
}

func assignCampusMemberToDefaultProject(ctx context.Context, client *ent.Client, userID int) error {
	defaultProject, err := client.Project.Query().
		Where(project.StatusEQ(project.StatusActive)).
		Order(ent.Asc(project.FieldID)).
		First(ctx)
	if err != nil {
		return fmt.Errorf("failed to find the campus sharing project: %w", err)
	}

	_, err = client.UserProject.Create().
		SetUserID(userID).
		SetProjectID(defaultProject.ID).
		SetScopes(campusMemberProjectScopes()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("failed to add user to the campus sharing project: %w", err)
	}

	return nil
}

type UserServiceParams struct {
	fx.In

	CacheConfig xcache.Config
	Ent         *ent.Client
}

type UserService struct {
	*AbstractService

	UserCache           xcache.Cache[ent.User]
	permissionValidator *PermissionValidator
}

func NewUserService(params UserServiceParams) *UserService {
	return &UserService{
		AbstractService: &AbstractService{
			db: params.Ent,
		},
		UserCache:           xcache.NewFromConfig[ent.User](params.CacheConfig),
		permissionValidator: NewPermissionValidator(),
	}
}

func requireSystemOwnerForDailyTokenLimit(ctx context.Context, dailyTokenLimit *int64) error {
	if dailyTokenLimit == nil {
		return nil
	}

	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil || !currentUser.IsOwner {
		return fmt.Errorf("daily token limit can only be changed by the system owner")
	}

	return nil
}

func requireSystemOwnerForOwnershipChange(ctx context.Context, isOwner *bool) error {
	if isOwner == nil {
		return nil
	}

	currentUser, ok := contexts.GetUser(ctx)
	if ok && currentUser != nil {
		if currentUser.IsOwner {
			return nil
		}

		return fmt.Errorf("system ownership can only be changed by the system owner")
	}

	principal, ok := authz.GetPrincipal(ctx)
	if ok && (principal.IsSystem() || principal.IsTest()) {
		return nil
	}

	return fmt.Errorf("system ownership can only be changed by the system owner")
}

// CreateUser creates a new user with hashed password.
func (s *UserService) CreateUser(ctx context.Context, input ent.CreateUserInput) (*ent.User, error) {
	if err := requireSystemOwnerForDailyTokenLimit(ctx, input.DailyTokenLimit); err != nil {
		return nil, err
	}
	if err := requireSystemOwnerForOwnershipChange(ctx, input.IsOwner); err != nil {
		return nil, err
	}

	normalizedEmail, err := normalizeCampusRegistrationEmail(input.Email)
	if err != nil {
		return nil, err
	}
	input.Email = normalizedEmail
	if input.Nickname != nil {
		nickname, err := NormalizeCampusNickname(*input.Nickname)
		if err != nil {
			return nil, err
		}
		input.Nickname = &nickname
	}

	// Hash the password
	var hashedPassword string
	if input.Password == OIDC_ONLY_PLACEHOLDER {
		hashedPassword = OIDC_ONLY_PLACEHOLDER
	} else {
		hashedPassword, err = HashPassword(input.Password)
		if err != nil {
			return nil, err
		}
	}

	var createdUser *ent.User
	err = s.RunInTransaction(ctx, func(txCtx context.Context) error {
		client := s.entFromContext(txCtx)
		mut := client.User.Create().
			SetNillableNickname(input.Nickname).
			SetNillableFirstName(input.FirstName).
			SetNillableLastName(input.LastName).
			SetEmail(input.Email).
			SetPassword(hashedPassword).
			SetNillableIsOwner(input.IsOwner).
			SetNillableDailyTokenLimit(input.DailyTokenLimit).
			SetScopes(input.Scopes)

		if input.RoleIDs != nil {
			mut.AddRoleIDs(input.RoleIDs...)
		}

		createdUser, err = mut.Save(txCtx)
		if err != nil {
			if ent.IsConstraintError(err) {
				return fmt.Errorf("%w: %v", ErrEmailAlreadyRegistered, err)
			}

			return fmt.Errorf("failed to create user: %w", err)
		}

		if !createdUser.IsOwner {
			if err := assignCampusMemberToDefaultProject(txCtx, client, createdUser.ID); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return createdUser, nil
}

// UpdateUser updates an existing user.
func (s *UserService) UpdateUser(ctx context.Context, id int, input ent.UpdateUserInput) (*ent.User, error) {
	if err := requireSystemOwnerForDailyTokenLimit(ctx, input.DailyTokenLimit); err != nil {
		return nil, err
	}
	if err := requireSystemOwnerForOwnershipChange(ctx, input.IsOwner); err != nil {
		return nil, err
	}
	if input.Nickname != nil {
		nickname, err := NormalizeCampusNickname(*input.Nickname)
		if err != nil {
			return nil, err
		}
		input.Nickname = &nickname
	}

	// Validate permissions before updating
	if err := s.permissionValidator.CanEditUserPermissions(ctx, id, nil); err != nil {
		return nil, fmt.Errorf("permission denied: %w", err)
	}

	// Validate scope grants if scopes are being updated
	if input.Scopes != nil {
		if err := s.permissionValidator.CanGrantScopes(ctx, input.Scopes, nil); err != nil {
			return nil, fmt.Errorf("permission denied: %w", err)
		}
	}
	if input.AppendScopes != nil {
		if err := s.permissionValidator.CanGrantScopes(ctx, input.AppendScopes, nil); err != nil {
			return nil, fmt.Errorf("permission denied: %w", err)
		}
	}

	// Validate role grants if roles are being added
	if input.AddRoleIDs != nil {
		for _, roleID := range input.AddRoleIDs {
			if err := s.permissionValidator.CanEditRole(ctx, roleID, nil); err != nil {
				return nil, fmt.Errorf("permission denied: %w", err)
			}
		}
	}

	client := s.entFromContext(ctx)
	if input.Email != nil {
		requestedEmail := strings.TrimSpace(*input.Email)
		normalizedEmail, err := normalizeCampusRegistrationEmail(requestedEmail)
		if err != nil {
			// Existing installations may have an owner account whose address predates
			// the campus-only registration policy. Allow an unchanged legacy address
			// to pass through an administrative edit without permitting any new
			// external address to be assigned.
			existingUser, queryErr := client.User.Get(ctx, id)
			if queryErr != nil {
				return nil, fmt.Errorf("load existing user for email validation: %w", queryErr)
			}
			if !strings.EqualFold(requestedEmail, strings.TrimSpace(existingUser.Email)) {
				return nil, err
			}
			input.Email = nil
		} else {
			input.Email = &normalizedEmail
		}
	}

	mut := client.User.UpdateOneID(id).
		SetNillableEmail(input.Email).
		SetNillableNickname(input.Nickname).
		SetNillableFirstName(input.FirstName).
		SetNillableLastName(input.LastName).
		SetNillableIsOwner(input.IsOwner).
		SetNillableDailyTokenLimit(input.DailyTokenLimit).
		SetNillablePreferLanguage(input.PreferLanguage)

	if input.ClearAvatar {
		mut.ClearAvatar()
	} else {
		mut.SetNillableAvatar(input.Avatar)
	}

	if input.Password != nil {
		hashedPassword, err := HashPassword(*input.Password)
		if err != nil {
			return nil, err
		}

		mut.SetPassword(hashedPassword)
	}

	if input.Scopes != nil {
		mut.SetScopes(input.Scopes)
	}

	if input.AppendScopes != nil {
		mut.AppendScopes(input.AppendScopes)
	}

	if input.ClearScopes {
		mut.ClearScopes()
	}

	if input.AddRoleIDs != nil {
		mut.AddRoleIDs(input.AddRoleIDs...)
	}

	if input.RemoveRoleIDs != nil {
		mut.RemoveRoleIDs(input.RemoveRoleIDs...)
	}

	if input.ClearRoles {
		mut.ClearRoles()
	}

	user, err := mut.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update user: %w", err)
	}

	// Invalidate cache
	s.invalidateUserCache(ctx, id)

	return user, nil
}

// UpdateOwnProfile updates fields users are allowed to change for their own account.
func (s *UserService) UpdateOwnProfile(ctx context.Context, input ent.UpdateUserInput) (*ent.User, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}

	id := currentUser.ID
	if input.Nickname != nil {
		nickname, err := NormalizeCampusNickname(*input.Nickname)
		if err != nil {
			return nil, err
		}
		input.Nickname = &nickname
	}

	return authz.RunWithSystemBypass(ctx, "update-own-profile", func(ctx context.Context) (*ent.User, error) {
		client := s.entFromContext(ctx)

		mut := client.User.UpdateOneID(id).
			SetNillableNickname(input.Nickname).
			SetNillableFirstName(input.FirstName).
			SetNillableLastName(input.LastName).
			SetNillablePreferLanguage(input.PreferLanguage)

		if input.ClearAvatar {
			mut.ClearAvatar()
		} else {
			mut.SetNillableAvatar(input.Avatar)
		}

		if input.Password != nil {
			hashedPassword, err := HashPassword(*input.Password)
			if err != nil {
				return nil, err
			}

			mut.SetPassword(hashedPassword)
		}

		user, err := mut.Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to update user profile: %w", err)
		}

		// Invalidate cache
		s.invalidateUserCache(ctx, id)

		return user, nil
	})
}

// UpdateUserStatus updates the status of a user.
func (s *UserService) UpdateUserStatus(ctx context.Context, id int, status user.Status) (*ent.User, error) {
	client := s.entFromContext(ctx)

	user, err := client.User.UpdateOneID(id).
		SetStatus(status).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update user status: %w", err)
	}

	// Invalidate cache
	s.invalidateUserCache(ctx, id)

	return user, nil
}

// GetUserByID gets a user by ID with caching.
func (s *UserService) GetUserByID(ctx context.Context, id int) (*ent.User, error) {
	// Try cache first
	cacheKey := buildUserCacheKey(id)
	if user, err := s.UserCache.Get(ctx, cacheKey); err == nil {
		return &user, nil
	}

	// Query database
	client := s.entFromContext(ctx)
	if client == nil {
		return nil, fmt.Errorf("ent client not found in context")
	}

	user, err := client.User.Query().
		Where(user.IDEQ(id)).
		WithRoles().
		WithProjects().
		WithProjectUsers().
		WithOidcIdentities().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	// Cache the user
	// TODO: handle role scope changed.
	err = s.UserCache.Set(ctx, cacheKey, *user)
	if err != nil {
		log.Warn(ctx, "failed to cache user", zap.Error(err))
	}

	return user, nil
}

func buildUserCacheKey(id int) string {
	return fmt.Sprintf("user:%d", id)
}

// invalidateUserCache removes a user from cache.
func (s *UserService) invalidateUserCache(ctx context.Context, id int) {
	cacheKey := buildUserCacheKey(id)
	_ = s.UserCache.Delete(ctx, cacheKey)
}

// clearUserCache clears all user cache.
func (s *UserService) clearUserCache(ctx context.Context) {
	_ = s.UserCache.Clear(ctx)
}

// ConvertUserToUserInfo converts ent.User to objects.UserInfo.
// This method handles the conversion of user data including roles, scopes, and projects.
// Note: This function panics if the provided user is nil.
func ConvertUserToUserInfo(ctx context.Context, u *ent.User) *objects.UserInfo {
	// Convert ent.Role to objects.RoleInfo (global roles only)
	userRoles := make([]objects.RoleInfo, 0)

	for _, r := range u.Edges.Roles {
		if !r.IsSystemRole() {
			// Skip project-specific roles, they will be included in project info
			continue
		}

		userRoles = append(userRoles, objects.RoleInfo{
			Name: r.Name,
		})
	}

	// Calculate all scopes (user scopes + global role scopes)
	allScopes := make(map[string]bool)

	// Add user's direct scopes
	for _, scope := range u.Scopes {
		allScopes[scope] = true
	}

	projectRoles := map[int][]*ent.Role{}

	// Add scopes from all global roles
	for _, r := range u.Edges.Roles {
		if !r.IsSystemRole() {
			projectRoles[*r.ProjectID] = append(projectRoles[*r.ProjectID], r)
			continue
		}

		for _, scope := range r.Scopes {
			allScopes[scope] = true
		}
	}

	// Convert user projects to objects.UserProjectInfo
	userProjects := make([]objects.UserProjectInfo, 0, len(u.Edges.ProjectUsers))

	for _, up := range u.Edges.ProjectUsers {
		// Convert project roles to objects.RoleInfo
		roles := projectRoles[up.ProjectID]

		projectRoleInfos := make([]objects.RoleInfo, 0, len(roles))
		for _, r := range roles {
			projectRoleInfos = append(projectRoleInfos, objects.RoleInfo{
				Name: r.Name,
			})
		}

		userProjects = append(userProjects, objects.UserProjectInfo{
			ProjectID: objects.GUID{Type: ent.TypeProject, ID: up.ProjectID},
			IsOwner:   up.IsOwner,
			Scopes:    up.Scopes,
			Roles:     projectRoleInfos,
		})
	}

	// Convert OIDC identities
	oidcIdentities := make([]objects.OIDCIdentityInfo, 0, len(u.Edges.OidcIdentities))
	for _, identity := range u.Edges.OidcIdentities {
		oidcIdentities = append(oidcIdentities, objects.OIDCIdentityInfo{
			ID:      objects.GUID{Type: ent.TypeOIDCIdentity, ID: identity.ID},
			IdpName: identity.IdpName,
			Issuer:  identity.Issuer,
			Subject: identity.Subject,
			Email:   identity.Email,
		})
	}

	return &objects.UserInfo{
		ID:             objects.GUID{Type: ent.TypeUser, ID: u.ID},
		Email:          u.Email,
		Nickname:       u.Nickname,
		FirstName:      u.FirstName,
		LastName:       u.LastName,
		IsOwner:        u.IsOwner,
		PreferLanguage: u.PreferLanguage,
		Avatar:         &u.Avatar,
		Scopes:         lo.Keys(allScopes),
		Roles:          userRoles,
		Projects:       userProjects,
		OIDCIdentities: oidcIdentities,
		HasPassword:    u.Password != OIDC_ONLY_PLACEHOLDER,
	}
}

// AddUserToProject adds a user to a project with optional owner status, scopes, and roles.
func (s *UserService) AddUserToProject(ctx context.Context, userID, projectID int, isOwner *bool, scopes []string, roleIDs []int) (*ent.UserProject, error) {
	client := s.entFromContext(ctx)

	// Create the project user relationship
	mut := client.UserProject.Create().
		SetUserID(userID).
		SetProjectID(projectID)

	if isOwner != nil {
		mut.SetIsOwner(*isOwner)
	}

	if scopes != nil {
		mut.SetScopes(scopes)
	}

	userProject, err := mut.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to add user to project: %w", err)
	}

	// Add roles if provided
	if len(roleIDs) > 0 {
		user, err := client.User.Get(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to get user: %w", err)
		}

		err = user.Update().AddRoleIDs(roleIDs...).Exec(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to add roles to user: %w", err)
		}
	}

	// Invalidate user cache
	s.invalidateUserCache(ctx, userID)

	return userProject, nil
}

// RemoveUserFromProject removes a user from a project.
func (s *UserService) RemoveUserFromProject(ctx context.Context, userID, projectID int) error {
	client := s.entFromContext(ctx)

	// Delete the relationship (soft delete if enabled)
	rowsAffected, err := client.UserProject.Delete().Where(
		userproject.ProjectIDEQ(projectID),
		userproject.UserIDEQ(userID),
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to remove user from project: %w", err)
	}

	if rowsAffected == 0 {
		return nil
	}

	projectRoleIDs, err := client.Role.Query().
		Where(
			role.ProjectIDEQ(projectID),
			role.HasUsersWith(user.IDEQ(userID)),
		).
		IDs(ctx)
	if err != nil {
		return fmt.Errorf("failed to query user project roles: %w", err)
	}

	if len(projectRoleIDs) > 0 {
		if err := client.User.UpdateOneID(userID).RemoveRoleIDs(projectRoleIDs...).Exec(ctx); err != nil {
			return fmt.Errorf("failed to remove user project roles: %w", err)
		}
	}

	// Invalidate user cache
	s.invalidateUserCache(ctx, userID)

	return nil
}

// UpdateProjectUser updates a user's project relationship including scopes and roles.
func (s *UserService) UpdateProjectUser(ctx context.Context, userID, projectID int, isOwner *bool, scopes []string, addRoleIDs, removeRoleIDs []int) (*ent.UserProject, error) {
	// Validate permissions before updating
	if err := s.permissionValidator.CanEditUserPermissions(ctx, userID, &projectID); err != nil {
		return nil, fmt.Errorf("permission denied: %w", err)
	}

	// Validate scope grants if scopes are being updated
	if scopes != nil {
		if err := s.permissionValidator.CanGrantScopes(ctx, scopes, &projectID); err != nil {
			return nil, fmt.Errorf("permission denied: %w", err)
		}
	}

	// Validate role grants if roles are being added
	if len(addRoleIDs) > 0 {
		for _, roleID := range addRoleIDs {
			if err := s.permissionValidator.CanEditRole(ctx, roleID, &projectID); err != nil {
				return nil, fmt.Errorf("permission denied: %w", err)
			}
		}
	}

	client := s.entFromContext(ctx)

	// Find the UserProject relationship
	userProject, err := client.UserProject.Query().
		Where(
			userproject.UserID(userID),
			userproject.ProjectID(projectID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find user project relationship: %w", err)
	}

	// Update the UserProject (including isOwner, scopes, and roles)
	mut := userProject.Update()

	if isOwner != nil {
		mut.SetIsOwner(*isOwner)
	}

	if scopes != nil {
		mut.SetScopes(scopes)
	}

	userProject, err = mut.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update project user: %w", err)
	}

	// Update roles if provided
	user, err := client.User.Get(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	userMut := user.Update()

	if len(addRoleIDs) > 0 {
		userMut.AddRoleIDs(addRoleIDs...)
	}

	if len(removeRoleIDs) > 0 {
		userMut.RemoveRoleIDs(removeRoleIDs...)
	}

	err = userMut.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update user roles: %w", err)
	}

	// Invalidate user cache
	s.invalidateUserCache(ctx, userID)

	return userProject, nil
}

// DeleteUser soft deletes a user and handles all related data.
// This method performs the following operations:
// 1. Validates permissions
// 2. Checks if user is owner (cannot delete owner)
// 3. Removes user from all projects (UserProject)
// 4. Removes all user roles (UserRole)
// 5. Soft deletes the user
// 6. Invalidates user cache.
func (s *UserService) DeleteUser(ctx context.Context, id int) error {
	// Validate permissions before deleting
	if err := s.permissionValidator.CanDeleteUser(ctx, id); err != nil {
		return fmt.Errorf("permission denied: %w", err)
	}

	return s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)

		// Get user to check if it's an owner
		u, err := client.User.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to get user: %w", err)
		}

		// Cannot delete owner users
		if u.IsOwner {
			return fmt.Errorf("cannot delete owner user, transfer ownership first")
		}

		// 1. Delete UserProject relationships
		_, err = client.UserProject.Delete().
			Where(userproject.UserIDEQ(id)).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to delete user projects: %w", err)
		}

		// 2. Delete UserRole relationships
		_, err = client.UserRole.Delete().
			Where(userrole.UserIDEQ(id)).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to delete user roles: %w", err)
		}

		// 3. Soft delete the user
		err = client.User.DeleteOneID(id).Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to delete user: %w", err)
		}

		// 4. Invalidate user cache
		s.invalidateUserCache(ctx, id)

		return nil
	})
}
