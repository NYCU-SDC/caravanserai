# Untitled object in Node Schema

```txt
node.json#/$defs/NodeStatus/properties/conditions/items
```

Condition describes a single observable aspect of a resource's state.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## items Type

`object` ([Details](node-defs-condition.md))

# items Properties

| Property                                  | Type     | Required | Nullable       | Defined by                                                                                                              |
| :---------------------------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------------------- |
| [type](#type)                             | `string` | Required | cannot be null | [Node](node-defs-condition-properties-type.md "node.json#/$defs/Condition/properties/type")                             |
| [status](#status)                         | `string` | Required | cannot be null | [Node](node-defs-condition-properties-status.md "node.json#/$defs/Condition/properties/status")                         |
| [lastHeartbeatTime](#lastheartbeattime)   | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-lastheartbeattime.md "node.json#/$defs/Condition/properties/lastHeartbeatTime")   |
| [lastTransitionTime](#lasttransitiontime) | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-lasttransitiontime.md "node.json#/$defs/Condition/properties/lastTransitionTime") |
| [reason](#reason)                         | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-reason.md "node.json#/$defs/Condition/properties/reason")                         |
| [message](#message)                       | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-message.md "node.json#/$defs/Condition/properties/message")                       |

## type

Type is a machine-readable identifier, e.g. "Ready", "Phase".

`type`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-type.md "node.json#/$defs/Condition/properties/type")

### type Type

`string`

## status

Status is one of True, False, Unknown.

`status`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-status.md "node.json#/$defs/Condition/properties/status")

### status Type

`string`

## lastHeartbeatTime

LastHeartbeatTime is when this condition was last sampled.

`lastHeartbeatTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-lastheartbeattime.md "node.json#/$defs/Condition/properties/lastHeartbeatTime")

### lastHeartbeatTime Type

`string`

### lastHeartbeatTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## lastTransitionTime

LastTransitionTime is when the Status last changed.

`lastTransitionTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-lasttransitiontime.md "node.json#/$defs/Condition/properties/lastTransitionTime")

### lastTransitionTime Type

`string`

### lastTransitionTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## reason

Reason is a CamelCase word summarising why the condition has this status.

`reason`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-reason.md "node.json#/$defs/Condition/properties/reason")

### reason Type

`string`

## message

Message is a human-readable explanation.

`message`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-message.md "node.json#/$defs/Condition/properties/message")

### message Type

`string`
