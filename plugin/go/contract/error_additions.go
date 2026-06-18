package contract

func ErrNotEligibleToPropose() *PluginError {
	return NewError(15, DefaultModule, "address must hold an active trust stake to propose a slash")
}

func ErrNotEligibleToVote() *PluginError {
	return NewError(16, DefaultModule, "address must hold an active trust stake to vote on a slash proposal")
}

func ErrProposalNotFound() *PluginError {
	return NewError(17, DefaultModule, "slash proposal not found")
}

func ErrProposalClosed() *PluginError {
	return NewError(18, DefaultModule, "slash proposal is no longer open for voting")
}

func ErrVotingPeriodEnded() *PluginError {
	return NewError(19, DefaultModule, "voting period for this proposal has ended")
}

func ErrVotingPeriodNotEnded() *PluginError {
	return NewError(20, DefaultModule, "voting period for this proposal has not yet ended")
}

func ErrAlreadyVoted() *PluginError {
	return NewError(21, DefaultModule, "address has already voted on this proposal")
}

func ErrInvalidProposalId() *PluginError {
	return NewError(22, DefaultModule, "proposal id is invalid")
}

func ErrStakeNotFound() *PluginError {
	return NewError(23, DefaultModule, "trust stake not found")
}

func ErrStakeAlreadySlashed() *PluginError {
	return NewError(24, DefaultModule, "trust stake has already been slashed")
}

func ErrActiveDisputeExists() *PluginError {
	return NewError(25, DefaultModule, "cannot withdraw stake while an active dispute exists against this target")
}

func ErrCooldownNotElapsed() *PluginError {
	return NewError(26, DefaultModule, "withdrawal cooldown period has not yet elapsed")
}
