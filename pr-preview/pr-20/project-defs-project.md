# Project Schema

```txt
project.json#/$defs/Project
```

Project is the central workload resource.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## Project Type

`object` ([Project](project-defs-project.md))

# Project Properties

| Property                  | Type     | Required | Nullable       | Defined by                                                                                                   |
| :------------------------ | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------- |
| [apiVersion](#apiversion) | `string` | Required | cannot be null | [Project](project-defs-project-properties-apiversion.md "project.json#/$defs/Project/properties/apiVersion") |
| [kind](#kind)             | `string` | Required | cannot be null | [Project](project-defs-project-properties-kind.md "project.json#/$defs/Project/properties/kind")             |
| [metadata](#metadata)     | `object` | Required | cannot be null | [Project](project-defs-objectmeta.md "project.json#/$defs/Project/properties/metadata")                      |
| [spec](#spec)             | `object` | Optional | cannot be null | [Project](project-defs-projectspec.md "project.json#/$defs/Project/properties/spec")                         |
| [status](#status)         | `object` | Optional | cannot be null | [Project](project-defs-projectstatus.md "project.json#/$defs/Project/properties/status")                     |

## apiVersion

APIVersion identifies the versioned schema this resource conforms to, e.g. "caravanserai/v1".

`apiVersion`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-project-properties-apiversion.md "project.json#/$defs/Project/properties/apiVersion")

### apiVersion Type

`string`

## kind

Kind is the resource type, e.g. "Node", "Project".

`kind`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-project-properties-kind.md "project.json#/$defs/Project/properties/kind")

### kind Type

`string`

## metadata

ObjectMeta holds identity and classification metadata common to all resources.

`metadata`

* is required

* Type: `object` ([Details](project-defs-objectmeta.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta.md "project.json#/$defs/Project/properties/metadata")

### metadata Type

`object` ([Details](project-defs-objectmeta.md))

## spec

ProjectSpec is the desired state declared by the user.

`spec`

* is optional

* Type: `object` ([Details](project-defs-projectspec.md))

* cannot be null

* defined in: [Project](project-defs-projectspec.md "project.json#/$defs/Project/properties/spec")

### spec Type

`object` ([Details](project-defs-projectspec.md))

## status

ProjectStatus is written by the Controller Manager and Agent.

`status`

* is optional

* Type: `object` ([Details](project-defs-projectstatus.md))

* cannot be null

* defined in: [Project](project-defs-projectstatus.md "project.json#/$defs/Project/properties/status")

### status Type

`object` ([Details](project-defs-projectstatus.md))
