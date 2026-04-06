# Untitled object in Node Schema

```txt
node.json#/$defs/ObjectMeta
```

ObjectMeta holds identity and classification metadata common to all resources.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## ObjectMeta Type

`object` ([Details](node-defs-objectmeta.md))

# ObjectMeta Properties

| Property                    | Type     | Required | Nullable       | Defined by                                                                                                  |
| :-------------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| [name](#name)               | `string` | Required | cannot be null | [Node](node-defs-objectmeta-properties-name.md "node.json#/$defs/ObjectMeta/properties/name")               |
| [labels](#labels)           | `object` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-labels.md "node.json#/$defs/ObjectMeta/properties/labels")           |
| [annotations](#annotations) | `object` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-annotations.md "node.json#/$defs/ObjectMeta/properties/annotations") |
| [createdAt](#createdat)     | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-createdat.md "node.json#/$defs/ObjectMeta/properties/createdAt")     |
| [updatedAt](#updatedat)     | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-updatedat.md "node.json#/$defs/ObjectMeta/properties/updatedAt")     |

## name

Name is the unique identifier within its Kind namespace.

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-name.md "node.json#/$defs/ObjectMeta/properties/name")

### name Type

`string`

## labels

Labels are arbitrary key/value pairs used for selection and grouping.

`labels`

* is optional

* Type: `object` ([Details](node-defs-objectmeta-properties-labels.md))

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-labels.md "node.json#/$defs/ObjectMeta/properties/labels")

### labels Type

`object` ([Details](node-defs-objectmeta-properties-labels.md))

## annotations

Annotations are non-identifying metadata (e.g. human-readable hints).

`annotations`

* is optional

* Type: `object` ([Details](node-defs-objectmeta-properties-annotations.md))

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-annotations.md "node.json#/$defs/ObjectMeta/properties/annotations")

### annotations Type

`object` ([Details](node-defs-objectmeta-properties-annotations.md))

## createdAt

CreatedAt is set by the server on first write.

`createdAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-createdat.md "node.json#/$defs/ObjectMeta/properties/createdAt")

### createdAt Type

`string`

### createdAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## updatedAt

UpdatedAt is set by the server on every write.

`updatedAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-updatedat.md "node.json#/$defs/ObjectMeta/properties/updatedAt")

### updatedAt Type

`string`

### updatedAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")
