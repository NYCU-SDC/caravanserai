# Untitled string in Node Schema

```txt
node.json#/$defs/NodeStatus/properties/lastHeartbeat
```

LastHeartbeat is the timestamp of the most recent heartbeat received
from the Agent. The NodeController uses this to detect timeouts.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## lastHeartbeat Type

`string`

## lastHeartbeat Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")
