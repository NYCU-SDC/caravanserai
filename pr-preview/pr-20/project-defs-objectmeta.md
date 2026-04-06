# Untitled object in Project Schema

```txt
project.json#/$defs/Project/properties/metadata
```

ObjectMeta holds identity and classification metadata common to all resources.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## metadata Type

`object` ([Details](project-defs-objectmeta.md))

# metadata Properties

| Property                    | Type     | Required | Nullable       | Defined by                                                                                                           |
| :-------------------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------------- |
| [name](#name)               | `string` | Required | cannot be null | [Project](project-defs-objectmeta-properties-name.md "project.json#/$defs/ObjectMeta/properties/name")               |
| [labels](#labels)           | `object` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-labels.md "project.json#/$defs/ObjectMeta/properties/labels")           |
| [annotations](#annotations) | `object` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-annotations.md "project.json#/$defs/ObjectMeta/properties/annotations") |
| [createdAt](#createdat)     | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-createdat.md "project.json#/$defs/ObjectMeta/properties/createdAt")     |
| [updatedAt](#updatedat)     | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-updatedat.md "project.json#/$defs/ObjectMeta/properties/updatedAt")     |

## name

Name is the unique identifier within its Kind namespace.

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-name.md "project.json#/$defs/ObjectMeta/properties/name")

### name Type

`string`

## labels

Labels are arbitrary key/value pairs used for selection and grouping.

`labels`

* is optional

* Type: `object` ([Details](project-defs-objectmeta-properties-labels.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-labels.md "project.json#/$defs/ObjectMeta/properties/labels")

### labels Type

`object` ([Details](project-defs-objectmeta-properties-labels.md))

## annotations

Annotations are non-identifying metadata (e.g. human-readable hints).

`annotations`

* is optional

* Type: `object` ([Details](project-defs-objectmeta-properties-annotations.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-annotations.md "project.json#/$defs/ObjectMeta/properties/annotations")

### annotations Type

`object` ([Details](project-defs-objectmeta-properties-annotations.md))

## createdAt

CreatedAt is set by the server on first write.

`createdAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-createdat.md "project.json#/$defs/ObjectMeta/properties/createdAt")

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

* defined in: [Project](project-defs-objectmeta-properties-updatedat.md "project.json#/$defs/ObjectMeta/properties/updatedAt")

### updatedAt Type

`string`

### updatedAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")
