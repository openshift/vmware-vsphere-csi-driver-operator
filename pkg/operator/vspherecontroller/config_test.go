package vspherecontroller

import "testing"

func TestINIConfig(t *testing.T) {
	tests := []struct {
		name           string
		config         string
		section        string
		key            string
		value          string
		expectedError  bool
		expectedString string
	}{
		{
			name: "Valid config with no changes",
			config: `[Global]
cluster-id = 1234
user       = user1
password   = password123`,
			section:       "",
			key:           "",
			value:         "",
			expectedError: false,
			expectedString: `[Global]
cluster-id = 1234
user       = user1
password   = password123`,
		},
		{
			name: "Valid config with double quotes",
			config: `[Global]
cluster-id = 1234
user       = "user1"
password   = "password123"`,
			section:       "",
			key:           "",
			value:         "",
			expectedError: false,
			expectedString: `[Global]
cluster-id = 1234
user       = user1
password   = password123`,
		},
		{
			name: "Valid config with inline comments",
			config: `[Global]
cluster-id = 1234
user       = user1
password   = password;123#456`,
			section:       "",
			key:           "",
			value:         "",
			expectedError: false,
			expectedString: `[Global]
cluster-id = 1234
user       = user1
password   = password;123#456`,
		},
		{
			name: "Valid config with changes",
			config: `[Global]
cluster-id = 1234
user       = user1
password   = password123`,
			section:       "Global",
			key:           "password",
			value:         "newpassword",
			expectedError: false,
			expectedString: `[Global]
cluster-id = 1234
user       = user1
password   = newpassword`,
		},
		{
			name: "Invalid config with unbalaced section",
			config: `[Global
cluster-id = 1234
user       = user1
password   = password123`,
			section:        "",
			key:            "",
			value:          "",
			expectedError:  true,
			expectedString: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iniCfg, err := newINIConfig(tt.config)
			if err == nil && tt.expectedError {
				t.Fatalf("expected error, got nothing")
			}
			if err != nil && !tt.expectedError {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.section != "" && tt.key != "" {
				iniCfg.Set(tt.section, tt.key, tt.value)
			}

			result := iniCfg.String()
			if tt.expectedString != result {
				t.Fatalf("expected:\n\n%s\ngot:\n\n%s", tt.expectedString, result)
			}
		})
	}
}
