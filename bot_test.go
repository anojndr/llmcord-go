package main

import "testing"

func TestCurrentModelForChannelIDsUsesLockedModelWithoutChangingGlobalModel(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.currentModel = firstTestModel

	var loadedConfig config

	loadedConfig.Models = map[string]map[string]any{
		firstTestModel:  nil,
		secondTestModel: nil,
	}
	loadedConfig.ModelOrder = []string{firstTestModel, secondTestModel}
	loadedConfig.ChannelModelLocks = map[string]string{"locked-channel": secondTestModel}

	currentModel := instance.currentModelForChannelIDs(loadedConfig, []string{"locked-channel"})
	if currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", currentModel)
	}

	if instance.currentModel != firstTestModel {
		t.Fatalf("unexpected global current model: %q", instance.currentModel)
	}
}

func TestCurrentModelForChannelIDsUsesFirstMatchingLock(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.currentModel = firstTestModel

	var loadedConfig config

	loadedConfig.Models = map[string]map[string]any{
		firstTestModel:  nil,
		secondTestModel: nil,
	}
	loadedConfig.ModelOrder = []string{firstTestModel, secondTestModel}
	loadedConfig.ChannelModelLocks = map[string]string{
		"thread-channel": secondTestModel,
		"parent-channel": firstTestModel,
	}

	currentModel := instance.currentModelForChannelIDs(
		loadedConfig,
		[]string{"thread-channel", "parent-channel"},
	)
	if currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", currentModel)
	}
}
