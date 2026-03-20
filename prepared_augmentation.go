package main

type preparedConversationAugmentation struct {
	warnings []string
	metadata *searchMetadata
	apply    func([]chatMessage) ([]chatMessage, error)
}

func emptyPreparedConversationAugmentation() preparedConversationAugmentation {
	return preparedConversationAugmentation{
		warnings: nil,
		metadata: nil,
		apply:    nil,
	}
}

func warningPreparedConversationAugmentation(
	warnings []string,
) preparedConversationAugmentation {
	return preparedConversationAugmentation{
		warnings: warnings,
		metadata: nil,
		apply:    nil,
	}
}

func newPreparedConversationAugmentation(
	warnings []string,
	metadata *searchMetadata,
	apply func([]chatMessage) ([]chatMessage, error),
) preparedConversationAugmentation {
	return preparedConversationAugmentation{
		warnings: warnings,
		metadata: metadata,
		apply:    apply,
	}
}

func applyPreparedConversationAugmentation(
	conversation []chatMessage,
	augmentation preparedConversationAugmentation,
) ([]chatMessage, error) {
	if augmentation.apply == nil {
		return conversation, nil
	}

	return augmentation.apply(conversation)
}
