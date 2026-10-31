use cosmwasm_schema::{cw_serde, QueryResponses};
use cosmwasm_std::HexBinary;

// Only the QueryResponses derive reads these, and it is compiled out on wasm.
#[cfg(not(target_arch = "wasm32"))]
use crate::state::{Config, Instance};

#[cw_serde]
pub struct InstantiateMsg {
    pub owner: String,
    pub writers: Vec<String>,
    pub max_endpoint_len: u32,
    pub max_topics: u32,
    pub max_instances_per_address: u32,
    pub min_update_interval_blocks: u64,
}

#[cw_serde]
pub enum ExecuteMsg {
    /// Writer only; upserts one instance of the sender. Expired instances of the sender do not
    /// count toward the limit and are replaced; then deletes expired instances oldest first.
    RegisterInstance {
        instance_id: String,
        endpoint: String,
        spki_sha256: HexBinary,
        protocol: u16,
        topics: Vec<String>,
    },
    /// Sender removes one instance of its own address; unknown ids are a no-op
    DeregisterInstance { instance_id: String },
    /// Writer only; deletes up to limit (capped at 100) expired instances, oldest first
    PruneExpired { limit: u32 },
    /// Owner only; replaces the list and deletes all instances of removed addresses
    SyncWriters { writers: Vec<String> },
    /// Owner only
    SetPaused { paused: bool },
    /// Owner only
    SetOwner { owner: String },
    /// Owner only; replaces every limit, validated as at instantiate. Stored instances are kept; see the README
    UpdateConfig {
        max_endpoint_len: u32,
        max_topics: u32,
        max_instances_per_address: u32,
        min_update_interval_blocks: u64,
        instance_ttl_blocks: u64,
        prune_per_write: u32,
    },
}

#[cw_serde]
pub struct MigrateMsg {}

/// Member, MembersOf and Members never return expired instances, pruned or not
#[cw_serde]
#[derive(QueryResponses)]
pub enum QueryMsg {
    #[returns(Config)]
    Config {},
    /// limit at most 100
    #[returns(Vec<String>)]
    Writers {
        start_after: Option<String>,
        limit: Option<u32>,
    },
    #[returns(Vec<Instance>)]
    Member { address: String },
    /// At most 100 addresses
    #[returns(Vec<(String, Vec<Instance>)>)]
    MembersOf { addresses: Vec<String> },
    /// start_after is (address, instance_id); limit at most 100
    #[returns(Vec<(String, Instance)>)]
    Members {
        start_after: Option<(String, String)>,
        limit: Option<u32>,
    },
}
