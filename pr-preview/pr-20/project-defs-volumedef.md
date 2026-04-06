# Untitled object in Project Schema

```txt
project.json#/$defs/VolumeDef
```

VolumeDef describes a named volume used by one or more services.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## VolumeDef Type

`object` ([Details](project-defs-volumedef.md))

# VolumeDef Properties

| Property      | Type     | Required | Nullable       | Defined by                                                                                           |
| :------------ | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------- |
| [name](#name) | `string` | Required | cannot be null | [Project](project-defs-volumedef-properties-name.md "project.json#/$defs/VolumeDef/properties/name") |
| [type](#type) | `string` | Required | cannot be null | [Project](project-defs-volumedef-properties-type.md "project.json#/$defs/VolumeDef/properties/type") |

## name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumedef-properties-name.md "project.json#/$defs/VolumeDef/properties/name")

### name Type

`string`

## type

Governs the lifecycle and backup behaviour of a Volume.

`type`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumedef-properties-type.md "project.json#/$defs/VolumeDef/properties/type")

### type Type

`string`

### type Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value         | Explanation |
| :------------ | :---------- |
| `"Ephemeral"` |             |
