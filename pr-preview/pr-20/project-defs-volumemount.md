# Untitled object in Project Schema

```txt
project.json#/$defs/VolumeMount
```

VolumeMount associates a named Volume with a container mount path.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## VolumeMount Type

`object` ([Details](project-defs-volumemount.md))

# VolumeMount Properties

| Property                | Type     | Required | Nullable       | Defined by                                                                                                         |
| :---------------------- | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------- |
| [name](#name)           | `string` | Required | cannot be null | [Project](project-defs-volumemount-properties-name.md "project.json#/$defs/VolumeMount/properties/name")           |
| [mountPath](#mountpath) | `string` | Required | cannot be null | [Project](project-defs-volumemount-properties-mountpath.md "project.json#/$defs/VolumeMount/properties/mountPath") |

## name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumemount-properties-name.md "project.json#/$defs/VolumeMount/properties/name")

### name Type

`string`

## mountPath



`mountPath`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumemount-properties-mountpath.md "project.json#/$defs/VolumeMount/properties/mountPath")

### mountPath Type

`string`
