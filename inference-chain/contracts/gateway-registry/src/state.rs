use cosmwasm_schema::cw_serde;
use cosmwasm_std::{Addr, HexBinary};
use cw_storage_plus::{Item, Map};

#[cw_serde]
pub struct Config {
    /// Only this account may SyncWriters, SetPaused and SetOwner
    pub owner: Addr,
    /// RegisterInstance is refused while paused; queries still answer
    pub paused: bool,
    pub max_endpoint_len: u32,
    pub max_topics: u32,
    pub max_instances_per_address: u32,
    /// Per instance
    pub min_update_interval_blocks: u64,
    /// An instance not refreshed for this long is expired
    pub instance_ttl_blocks: u64,
    /// Expired instances one RegisterInstance deletes on the way
    pub prune_per_write: u32,
    pub writers_source: WritersSource,
}

#[cw_serde]
pub enum WritersSource {
    ContractCopy,
    ChainQuery,
}

#[cw_serde]
pub struct Instance {
    /// [a-z0-9-]+, at most 32 bytes, unique per address
    pub instance_id: String,
    /// Advertised "https://host:port" of this instance's peer listener
    pub endpoint: String,
    /// SHA-256 of this instance's transport key, DER SubjectPublicKeyInfo
    pub spki_sha256: HexBinary,
    pub protocol: u16,
    /// [a-z0-9_]+@[0-9]+, at most 32 bytes each; opaque to the contract
    pub topics: Vec<String>,
    /// The rate limit and the expiry count from here
    pub updated_height: u64,
}

pub const CONFIG: Item<Config> = Item::new("config");
pub const WRITERS: Map<&Addr, ()> = Map::new("writers");
/// Height of the last SyncWriters
pub const WRITERS_SYNCED_AT: Item<u64> = Item::new("writers_synced_at");
pub const INSTANCES: Map<(&Addr, &str), Instance> = Map::new("instances");
/// (updated_height, address, instance_id): oldest first, for pruning
pub const BY_HEIGHT: Map<(u64, &Addr, &str), ()> = Map::new("by_height");
