use cosmwasm_std::StdError;
use thiserror::Error;

#[derive(Error, Debug)]
pub enum ContractError {
    #[error("{0}")]
    Std(#[from] StdError),

    #[error("Unauthorized")]
    Unauthorized {},

    #[error("Contract is paused")]
    Paused {},

    #[error("Sender is not a writer")]
    NotAWriter {},

    #[error("Instance updated too recently, next update at height {next_height}")]
    TooSoon { next_height: u64 },

    #[error("Bad instance id")]
    BadInstanceId {},

    #[error("Too many instances, max {max}")]
    TooManyInstances { max: u32 },

    #[error("Bad endpoint")]
    BadEndpoint {},

    #[error("Fingerprint must be 32 bytes")]
    BadFingerprint {},

    #[error("Bad topic: {topic}")]
    BadTopic { topic: String },

    #[error("Too many topics")]
    TooManyTopics {},

    #[error("Too many addresses")]
    TooManyAddresses {},

    #[error("Invalid config: {reason}")]
    InvalidConfig { reason: String },
}
