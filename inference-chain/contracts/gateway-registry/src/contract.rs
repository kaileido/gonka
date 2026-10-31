use std::collections::BTreeSet;

use cosmwasm_std::{
    entry_point, to_json_binary, Addr, Api, Binary, Deps, DepsMut, Env, Event, HexBinary,
    MessageInfo, Order, Response, StdResult, Storage,
};
use cw2::set_contract_version;
use cw_storage_plus::Bound;

use crate::error::ContractError;
use crate::msg::{ExecuteMsg, InstantiateMsg, MigrateMsg, QueryMsg};
use crate::state::{
    Config, Instance, WritersSource, BY_HEIGHT, CONFIG, INSTANCES, WRITERS, WRITERS_SYNCED_AT,
};

const CONTRACT_NAME: &str = "gateway-registry";
const CONTRACT_VERSION: &str = env!("CARGO_PKG_VERSION");

const INSTANCE_TTL_BLOCKS: u64 = 16200;
const MAX_INSTANCE_TTL_BLOCKS: u64 = 5_913_000;
const PRUNE_PER_WRITE: u32 = 10;
const MAX_PRUNE_LIMIT: u32 = 100;
const MAX_PAGE_LIMIT: u32 = 100;
const MAX_MEMBERS_OF_ADDRESSES: usize = 100;
const MAX_INSTANCE_ID_LEN: usize = 32;
const MAX_TOPIC_LEN: usize = 32;
const SPKI_SHA256_LEN: usize = 32;

#[entry_point]
pub fn instantiate(
    deps: DepsMut,
    env: Env,
    _info: MessageInfo,
    msg: InstantiateMsg,
) -> Result<Response, ContractError> {
    set_contract_version(deps.storage, CONTRACT_NAME, CONTRACT_VERSION)?;

    let config = Config {
        owner: deps.api.addr_validate(&msg.owner)?,
        paused: false,
        max_endpoint_len: msg.max_endpoint_len,
        max_topics: msg.max_topics,
        max_instances_per_address: msg.max_instances_per_address,
        min_update_interval_blocks: msg.min_update_interval_blocks,
        instance_ttl_blocks: INSTANCE_TTL_BLOCKS,
        prune_per_write: PRUNE_PER_WRITE,
        writers_source: WritersSource::ContractCopy,
    };
    validate_config(&config)?;
    CONFIG.save(deps.storage, &config)?;

    for writer in validate_writers(deps.api, &msg.writers)? {
        WRITERS.save(deps.storage, &writer, &())?;
    }
    WRITERS_SYNCED_AT.save(deps.storage, &env.block.height)?;

    Ok(Response::new().add_attribute("action", "instantiate"))
}

#[entry_point]
pub fn execute(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    msg: ExecuteMsg,
) -> Result<Response, ContractError> {
    match msg {
        ExecuteMsg::RegisterInstance {
            instance_id,
            endpoint,
            spki_sha256,
            protocol,
            topics,
        } => register_instance(
            deps,
            env,
            info,
            instance_id,
            endpoint,
            spki_sha256,
            protocol,
            topics,
        ),
        ExecuteMsg::DeregisterInstance { instance_id } => {
            deregister_instance(deps, info, instance_id)
        }
        ExecuteMsg::PruneExpired { limit } => prune_expired(deps, env, info, limit),
        ExecuteMsg::SyncWriters { writers } => sync_writers(deps, env, info, writers),
        ExecuteMsg::SetPaused { paused } => set_paused(deps, info, paused),
        ExecuteMsg::SetOwner { owner } => set_owner(deps, info, owner),
        ExecuteMsg::UpdateConfig {
            max_endpoint_len,
            max_topics,
            max_instances_per_address,
            min_update_interval_blocks,
            instance_ttl_blocks,
            prune_per_write,
        } => update_config(
            deps,
            info,
            max_endpoint_len,
            max_topics,
            max_instances_per_address,
            min_update_interval_blocks,
            instance_ttl_blocks,
            prune_per_write,
        ),
    }
}

#[allow(clippy::too_many_arguments)]
fn register_instance(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    instance_id: String,
    endpoint: String,
    spki_sha256: HexBinary,
    protocol: u16,
    topics: Vec<String>,
) -> Result<Response, ContractError> {
    let config = CONFIG.load(deps.storage)?;
    if config.paused {
        return Err(ContractError::Paused {});
    }
    ensure_writer(deps.storage, &info.sender)?;
    validate_instance_id(&instance_id)?;
    validate_endpoint(&endpoint, config.max_endpoint_len)?;
    if spki_sha256.len() != SPKI_SHA256_LEN {
        return Err(ContractError::BadFingerprint {});
    }
    validate_topics(&topics, config.max_topics)?;

    let height = env.block.height;
    let mut response = Response::new();
    let mut prune_budget = config.prune_per_write;

    match INSTANCES.may_load(deps.storage, (&info.sender, &instance_id))? {
        Some(existing) => {
            let next_height = existing.updated_height + config.min_update_interval_blocks;
            if height < next_height {
                return Err(ContractError::TooSoon { next_height });
            }
            remove_instance(
                deps.storage,
                &info.sender,
                &existing.instance_id,
                existing.updated_height,
            );
        }
        None => {
            let stored = load_instances(deps.storage, &info.sender)?;
            let excess =
                (stored.len() + 1).saturating_sub(config.max_instances_per_address as usize);
            if excess > 0 {
                // The address stays within the limit on disk: oldest expired instances make room.
                let mut expired: Vec<&Instance> = stored
                    .iter()
                    .filter(|instance| is_expired(instance, height, &config))
                    .collect();
                if expired.len() < excess {
                    return Err(ContractError::TooManyInstances {
                        max: config.max_instances_per_address,
                    });
                }
                expired.sort_by_key(|instance| instance.updated_height);
                for oldest_expired in &expired[..excess] {
                    remove_instance(
                        deps.storage,
                        &info.sender,
                        &oldest_expired.instance_id,
                        oldest_expired.updated_height,
                    );
                    response = response
                        .add_event(expired_event(&info.sender, &oldest_expired.instance_id));
                }
                prune_budget = prune_budget.saturating_sub(excess as u32);
            }
        }
    }

    let instance = Instance {
        instance_id,
        endpoint,
        spki_sha256,
        protocol,
        topics,
        updated_height: height,
    };
    INSTANCES.save(
        deps.storage,
        (&info.sender, &instance.instance_id),
        &instance,
    )?;
    BY_HEIGHT.save(
        deps.storage,
        (height, &info.sender, &instance.instance_id),
        &(),
    )?;

    let registered = Event::new("instance_registered")
        .add_attribute("address", info.sender.as_str())
        .add_attribute("instance_id", &instance.instance_id)
        .add_attribute("endpoint", &instance.endpoint)
        .add_attribute("spki_sha256", instance.spki_sha256.to_hex())
        .add_attribute("protocol", instance.protocol.to_string());
    let pruned = delete_expired(deps.storage, height, &config, prune_budget)?;

    Ok(response.add_event(registered).add_events(pruned))
}

fn deregister_instance(
    deps: DepsMut,
    info: MessageInfo,
    instance_id: String,
) -> Result<Response, ContractError> {
    let mut response = Response::new();
    if let Some(instance) = INSTANCES.may_load(deps.storage, (&info.sender, &instance_id))? {
        remove_instance(
            deps.storage,
            &info.sender,
            &instance_id,
            instance.updated_height,
        );
        response = response.add_event(deregistered_event(&info.sender, &instance_id));
    }
    Ok(response)
}

fn prune_expired(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    limit: u32,
) -> Result<Response, ContractError> {
    let config = CONFIG.load(deps.storage)?;
    ensure_writer(deps.storage, &info.sender)?;
    let pruned = delete_expired(
        deps.storage,
        env.block.height,
        &config,
        limit.min(MAX_PRUNE_LIMIT),
    )?;
    Ok(Response::new().add_events(pruned))
}

fn sync_writers(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    writers: Vec<String>,
) -> Result<Response, ContractError> {
    ensure_owner(deps.storage, &info.sender)?;
    let writers = validate_writers(deps.api, &writers)?;

    let previous = WRITERS
        .keys(deps.storage, None, None, Order::Ascending)
        .collect::<StdResult<Vec<_>>>()?;
    let mut response = Response::new();
    for address in previous.iter().filter(|a| !writers.contains(*a)) {
        WRITERS.remove(deps.storage, address);
        for instance in load_instances(deps.storage, address)? {
            remove_instance(
                deps.storage,
                address,
                &instance.instance_id,
                instance.updated_height,
            );
            response = response.add_event(deregistered_event(address, &instance.instance_id));
        }
    }
    for writer in &writers {
        WRITERS.save(deps.storage, writer, &())?;
    }
    WRITERS_SYNCED_AT.save(deps.storage, &env.block.height)?;

    Ok(response.add_event(
        Event::new("writers_synced")
            .add_attribute("count", writers.len().to_string())
            .add_attribute("height", env.block.height.to_string()),
    ))
}

fn set_paused(deps: DepsMut, info: MessageInfo, paused: bool) -> Result<Response, ContractError> {
    let mut config = ensure_owner(deps.storage, &info.sender)?;
    config.paused = paused;
    CONFIG.save(deps.storage, &config)?;
    Ok(Response::new()
        .add_attribute("action", "set_paused")
        .add_attribute("paused", paused.to_string()))
}

fn set_owner(deps: DepsMut, info: MessageInfo, owner: String) -> Result<Response, ContractError> {
    let mut config = ensure_owner(deps.storage, &info.sender)?;
    config.owner = deps.api.addr_validate(&owner)?;
    CONFIG.save(deps.storage, &config)?;
    Ok(Response::new()
        .add_attribute("action", "set_owner")
        .add_attribute("owner", config.owner.as_str()))
}

#[allow(clippy::too_many_arguments)]
fn update_config(
    deps: DepsMut,
    info: MessageInfo,
    max_endpoint_len: u32,
    max_topics: u32,
    max_instances_per_address: u32,
    min_update_interval_blocks: u64,
    instance_ttl_blocks: u64,
    prune_per_write: u32,
) -> Result<Response, ContractError> {
    let config = Config {
        max_endpoint_len,
        max_topics,
        max_instances_per_address,
        min_update_interval_blocks,
        instance_ttl_blocks,
        prune_per_write,
        ..ensure_owner(deps.storage, &info.sender)?
    };
    validate_config(&config)?;
    CONFIG.save(deps.storage, &config)?;
    Ok(Response::new().add_attribute("action", "update_config"))
}

#[entry_point]
pub fn query(deps: Deps, env: Env, msg: QueryMsg) -> Result<Binary, ContractError> {
    let binary = match msg {
        QueryMsg::Config {} => to_json_binary(&CONFIG.load(deps.storage)?)?,
        QueryMsg::Writers { start_after, limit } => {
            to_json_binary(&query_writers(deps, start_after, limit)?)?
        }
        QueryMsg::Member { address } => {
            let address = deps.api.addr_validate(&address)?;
            to_json_binary(&live_instances(deps, &env, &address)?)?
        }
        QueryMsg::MembersOf { addresses } => {
            to_json_binary(&query_members_of(deps, &env, addresses)?)?
        }
        QueryMsg::Members { start_after, limit } => {
            to_json_binary(&query_members(deps, &env, start_after, limit)?)?
        }
    };
    Ok(binary)
}

fn query_writers(
    deps: Deps,
    start_after: Option<String>,
    limit: Option<u32>,
) -> Result<Vec<String>, ContractError> {
    let start_after = start_after
        .map(|address| deps.api.addr_validate(&address))
        .transpose()?;
    let start = start_after.as_ref().map(Bound::exclusive);
    let writers = WRITERS
        .keys(deps.storage, start, None, Order::Ascending)
        .take(page_limit(limit))
        .map(|address| address.map(Addr::into_string))
        .collect::<StdResult<Vec<_>>>()?;
    Ok(writers)
}

fn query_members_of(
    deps: Deps,
    env: &Env,
    addresses: Vec<String>,
) -> Result<Vec<(String, Vec<Instance>)>, ContractError> {
    if addresses.len() > MAX_MEMBERS_OF_ADDRESSES {
        return Err(ContractError::TooManyAddresses {});
    }
    addresses
        .into_iter()
        .map(|address| {
            let validated = deps.api.addr_validate(&address)?;
            let instances = live_instances(deps, env, &validated)?;
            Ok((address, instances))
        })
        .collect()
}

fn query_members(
    deps: Deps,
    env: &Env,
    start_after: Option<(String, String)>,
    limit: Option<u32>,
) -> Result<Vec<(String, Instance)>, ContractError> {
    let config = CONFIG.load(deps.storage)?;
    let start_after = start_after
        .map(|(address, id)| deps.api.addr_validate(&address).map(|a| (a, id)))
        .transpose()?;
    let start = start_after
        .as_ref()
        .map(|(address, id)| Bound::exclusive((address, id.as_str())));

    let mut members = Vec::new();
    for item in INSTANCES.range(deps.storage, start, None, Order::Ascending) {
        if members.len() == page_limit(limit) {
            break;
        }
        let ((address, _), instance) = item?;
        if !is_expired(&instance, env.block.height, &config) {
            members.push((address.into_string(), instance));
        }
    }
    Ok(members)
}

fn live_instances(deps: Deps, env: &Env, address: &Addr) -> Result<Vec<Instance>, ContractError> {
    let config = CONFIG.load(deps.storage)?;
    let mut instances = load_instances(deps.storage, address)?;
    instances.retain(|instance| !is_expired(instance, env.block.height, &config));
    Ok(instances)
}

fn page_limit(limit: Option<u32>) -> usize {
    limit.unwrap_or(MAX_PAGE_LIMIT).min(MAX_PAGE_LIMIT) as usize
}

#[entry_point]
pub fn migrate(deps: DepsMut, _env: Env, _msg: MigrateMsg) -> Result<Response, ContractError> {
    set_contract_version(deps.storage, CONTRACT_NAME, CONTRACT_VERSION)?;
    Ok(Response::new().add_attribute("action", "migrate"))
}

/// Rejects a config under which instances could not be stored, pruned or refreshed before expiry.
pub fn validate_config(config: &Config) -> Result<(), ContractError> {
    let reason = if config.instance_ttl_blocks == 0 {
        "instance_ttl_blocks must be positive"
    } else if config.instance_ttl_blocks > MAX_INSTANCE_TTL_BLOCKS {
        "instance_ttl_blocks is over its limit"
    } else if config.prune_per_write == 0 {
        "prune_per_write must be positive"
    } else if config.max_instances_per_address == 0 {
        "max_instances_per_address must be positive"
    } else if config.min_update_interval_blocks >= config.instance_ttl_blocks {
        "min_update_interval_blocks must be below instance_ttl_blocks"
    } else {
        return Ok(());
    };
    Err(ContractError::InvalidConfig {
        reason: reason.to_string(),
    })
}

fn ensure_owner(storage: &dyn Storage, sender: &Addr) -> Result<Config, ContractError> {
    let config = CONFIG.load(storage)?;
    if *sender != config.owner {
        return Err(ContractError::Unauthorized {});
    }
    Ok(config)
}

fn ensure_writer(storage: &dyn Storage, sender: &Addr) -> Result<(), ContractError> {
    if !WRITERS.has(storage, sender) {
        return Err(ContractError::NotAWriter {});
    }
    Ok(())
}

fn is_expired(instance: &Instance, height: u64, config: &Config) -> bool {
    height - instance.updated_height >= config.instance_ttl_blocks
}

fn load_instances(storage: &dyn Storage, address: &Addr) -> StdResult<Vec<Instance>> {
    INSTANCES
        .prefix(address)
        .range(storage, None, None, Order::Ascending)
        .map(|item| item.map(|(_, instance)| instance))
        .collect()
}

// Every delete goes through here so BY_HEIGHT never keeps an orphan row.
fn remove_instance(
    storage: &mut dyn Storage,
    address: &Addr,
    instance_id: &str,
    updated_height: u64,
) {
    INSTANCES.remove(storage, (address, instance_id));
    BY_HEIGHT.remove(storage, (updated_height, address, instance_id));
}

// Oldest first; stops at the first live row, so the reads stay bounded by limit.
fn delete_expired(
    storage: &mut dyn Storage,
    height: u64,
    config: &Config,
    limit: u32,
) -> StdResult<Vec<Event>> {
    let expired = BY_HEIGHT
        .keys(storage, None, None, Order::Ascending)
        .take_while(|key| {
            key.as_ref().map_or(true, |(updated, _, _)| {
                height - updated >= config.instance_ttl_blocks
            })
        })
        .take(limit as usize)
        .collect::<StdResult<Vec<_>>>()?;

    let mut events = Vec::with_capacity(expired.len());
    for (updated, address, instance_id) in expired {
        remove_instance(storage, &address, &instance_id, updated);
        events.push(expired_event(&address, &instance_id));
    }
    Ok(events)
}

fn expired_event(address: &Addr, instance_id: &str) -> Event {
    Event::new("instance_expired")
        .add_attribute("address", address.as_str())
        .add_attribute("instance_id", instance_id)
}

fn deregistered_event(address: &Addr, instance_id: &str) -> Event {
    Event::new("instance_deregistered")
        .add_attribute("address", address.as_str())
        .add_attribute("instance_id", instance_id)
}

fn validate_writers(api: &dyn Api, writers: &[String]) -> StdResult<BTreeSet<Addr>> {
    writers.iter().map(|w| api.addr_validate(w)).collect()
}

fn validate_instance_id(instance_id: &str) -> Result<(), ContractError> {
    let valid = !instance_id.is_empty()
        && instance_id.len() <= MAX_INSTANCE_ID_LEN
        && instance_id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-');
    if !valid {
        return Err(ContractError::BadInstanceId {});
    }
    Ok(())
}

// Exactly "https://host:port": no path, query or userinfo. IPv6 hosts are bracketed.
fn validate_endpoint(endpoint: &str, max_len: u32) -> Result<(), ContractError> {
    let valid = endpoint.len() <= max_len as usize
        && endpoint
            .strip_prefix("https://")
            .and_then(|rest| rest.rsplit_once(':'))
            .is_some_and(|(host, port)| valid_host(host) && valid_port(port));
    if !valid {
        return Err(ContractError::BadEndpoint {});
    }
    Ok(())
}

fn valid_host(host: &str) -> bool {
    match host.strip_prefix('[').and_then(|h| h.strip_suffix(']')) {
        Some(ipv6) => !ipv6.is_empty() && ipv6.bytes().all(|b| b.is_ascii_hexdigit() || b == b':'),
        None => {
            !host.is_empty()
                && host
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
        }
    }
}

// The digit check rejects "+443", which u16::from_str accepts.
fn valid_port(port: &str) -> bool {
    port.bytes().all(|b| b.is_ascii_digit()) && port.parse::<u16>().is_ok_and(|p| p != 0)
}

fn validate_topics(topics: &[String], max_topics: u32) -> Result<(), ContractError> {
    if topics.len() > max_topics as usize {
        return Err(ContractError::TooManyTopics {});
    }
    for topic in topics {
        let valid = topic.len() <= MAX_TOPIC_LEN
            && topic.split_once('@').is_some_and(|(name, version)| {
                !name.is_empty()
                    && name
                        .bytes()
                        .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
                    && !version.is_empty()
                    && version.bytes().all(|b| b.is_ascii_digit())
            });
        if !valid {
            return Err(ContractError::BadTopic {
                topic: topic.clone(),
            });
        }
    }
    Ok(())
}
