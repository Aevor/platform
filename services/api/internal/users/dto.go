package users

type CreateUserRequest struct {
	GithubID int64 `json:"github_id"`

	Username string `json:"username"`

	DisplayName string `json:"display_name"`

	Email string `json:"email"`

	AvatarURL string `json:"avatar_url"`
}

type UserResponse struct {
	ID string `json:"id"`

	GithubID int64 `json:"github_id"`

	Username string `json:"username"`

	DisplayName string `json:"display_name"`

	Email string `json:"email"`

	AvatarURL string `json:"avatar_url"`
}

func ToUserResponse(user *User) UserResponse {
	return UserResponse{
		ID:          user.ID.String(),
		GithubID:    user.GithubID,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Email:       user.Email,
		AvatarURL:   user.AvatarURL,
	}
}
