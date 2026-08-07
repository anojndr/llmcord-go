package main

import "testing"

func TestMessageAllowed(t *testing.T) {
	t.Parallel()

	loadedConfig := testPermissionsConfig()

	testCases := []struct {
		name     string
		context  accessContext
		expected bool
	}{
		{
			name: "admin can use bot in blocked dms",
			context: accessContext{
				IsDM:       true,
				UserID:     "admin-user",
				RoleIDs:    nil,
				ChannelIDs: nil,
			},
			expected: true,
		},
		{
			name: "blocked user is denied",
			context: accessContext{
				IsDM:       true,
				UserID:     "blocked-user",
				RoleIDs:    nil,
				ChannelIDs: nil,
			},
			expected: false,
		},
		{
			name: "allowed role can access guild channel",
			context: accessContext{
				IsDM:       false,
				UserID:     "member-user",
				RoleIDs:    []string{"allowed-role"},
				ChannelIDs: []string{"allowed-channel"},
			},
			expected: true,
		},
		{
			name: "blocked channel wins over allow",
			context: accessContext{
				IsDM:       false,
				UserID:     "allowed-user",
				RoleIDs:    nil,
				ChannelIDs: []string{"allowed-channel", "blocked-channel"},
			},
			expected: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			allowed := messageAllowed(loadedConfig, testCase.context)
			if allowed != testCase.expected {
				t.Fatalf("unexpected access result: got %t want %t", allowed, testCase.expected)
			}
		})
	}
}

func testPermissionsConfig() config {
	searchConfig := webSearchConfig{
		PrimaryProvider: webSearchProviderKindMCP,
		MaxURLs:         defaultWebSearchMaxURLs,
		Freeweb:         freewebSearchConfig{Enabled: false},
		Exa: exaSearchConfig{
			APIKey:             "",
			APIKeys:            nil,
			SearchType:         defaultExaSearchType,
			TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
			LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
		},
		Tavily: tavilySearchConfig{
			APIKey:  "",
			APIKeys: nil,
		},
	}

	return config{
		BotToken:      "",
		ClientID:      "",
		StatusMessage: "",
		MaxImages:     0,
		MaxMessages:   0,
		AllowDMs:      false,
		Permissions: permissionsConfig{
			Users: userPermissions{
				AdminIDs:   idList{"admin-user"},
				AllowedIDs: idList{"allowed-user"},
				BlockedIDs: idList{"blocked-user"},
			},
			Roles: scopePermissions{
				AllowedIDs: idList{"allowed-role"},
				BlockedIDs: idList{"blocked-role"},
			},
			Channels: scopePermissions{
				AllowedIDs: idList{"allowed-channel"},
				BlockedIDs: idList{"blocked-channel"},
			},
		},
		Providers: nil,
		WebSearch: searchConfig,
		Database: databaseConfig{
			ConnectionString: "",
			StoreKey:         "",
		},
		Gist: gistConfig{
			Endpoint:    "",
			APIKey:      "",
			APIKeys:     nil,
			Description: "",
			Filename:    defaultGistFilename,
			Public:      false,
		},
		VisualSearch: visualSearchConfig{
			SerpAPI: serpAPIVisualSearchConfig{APIKey: "", APIKeys: nil},
		},
		Models:                 nil,
		ModelContextWindows:    map[string]int{},
		ProviderContextWindows: nil,
		ModelOrder:             nil,
		ChannelModelLocks:      nil,
		SearchDeciderModel:     "",
		MediaAnalysisModel:     "",
		SystemPrompt:           "",
	}
}
