# Untitled object in Project Schema

```txt
project.json#/$defs/ServiceDef
```

ServiceDef describes a single container (analogous to a Compose service).

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## ServiceDef Type

`object` ([Details](project-defs-servicedef.md))

# ServiceDef Properties

| Property                      | Type     | Required | Nullable       | Defined by                                                                                                             |
| :---------------------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------------------- |
| [name](#name)                 | `string` | Required | cannot be null | [Project](project-defs-servicedef-properties-name.md "project.json#/$defs/ServiceDef/properties/name")                 |
| [image](#image)               | `string` | Required | cannot be null | [Project](project-defs-servicedef-properties-image.md "project.json#/$defs/ServiceDef/properties/image")               |
| [env](#env)                   | `array`  | Optional | cannot be null | [Project](project-defs-servicedef-properties-env.md "project.json#/$defs/ServiceDef/properties/env")                   |
| [volumeMounts](#volumemounts) | `array`  | Optional | cannot be null | [Project](project-defs-servicedef-properties-volumemounts.md "project.json#/$defs/ServiceDef/properties/volumeMounts") |

## name

Name identifies the service within the Project (used as the DNS
hostname inside the shared bridge network).

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-name.md "project.json#/$defs/ServiceDef/properties/name")

### name Type

`string`

## image

Image is the Docker image reference, e.g. "postgres:15".

`image`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-image.md "project.json#/$defs/ServiceDef/properties/image")

### image Type

`string`

## env

Env are extra environment variables injected at runtime.

`env`

* is optional

* Type: `object[]` ([Details](project-defs-envvar.md))

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-env.md "project.json#/$defs/ServiceDef/properties/env")

### env Type

`object[]` ([Details](project-defs-envvar.md))

## volumeMounts

VolumeMounts lists volumes to attach to this container.

`volumeMounts`

* is optional

* Type: `object[]` ([Details](project-defs-volumemount.md))

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-volumemounts.md "project.json#/$defs/ServiceDef/properties/volumeMounts")

### volumeMounts Type

`object[]` ([Details](project-defs-volumemount.md))
