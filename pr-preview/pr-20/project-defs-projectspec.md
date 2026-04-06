# Untitled object in Project Schema

```txt
project.json#/$defs/ProjectSpec
```

ProjectSpec is the desired state declared by the user.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## ProjectSpec Type

`object` ([Details](project-defs-projectspec.md))

# ProjectSpec Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                       |
| :-------------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------------- |
| [services](#services) | `array`  | Required | cannot be null | [Project](project-defs-projectspec-properties-services.md "project.json#/$defs/ProjectSpec/properties/services") |
| [volumes](#volumes)   | `array`  | Optional | cannot be null | [Project](project-defs-projectspec-properties-volumes.md "project.json#/$defs/ProjectSpec/properties/volumes")   |
| [ingress](#ingress)   | `array`  | Optional | cannot be null | [Project](project-defs-projectspec-properties-ingress.md "project.json#/$defs/ProjectSpec/properties/ingress")   |
| [expireAt](#expireat) | `string` | Optional | cannot be null | [Project](project-defs-projectspec-properties-expireat.md "project.json#/$defs/ProjectSpec/properties/expireAt") |

## services

Services is the ordered list of containers to run.

`services`

* is required

* Type: `object[]` ([Details](project-defs-servicedef.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-services.md "project.json#/$defs/ProjectSpec/properties/services")

### services Type

`object[]` ([Details](project-defs-servicedef.md))

## volumes

Volumes are named storage units shared across services.

`volumes`

* is optional

* Type: `object[]` ([Details](project-defs-volumedef.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-volumes.md "project.json#/$defs/ProjectSpec/properties/volumes")

### volumes Type

`object[]` ([Details](project-defs-volumedef.md))

## ingress

Ingress defines public or internal HTTP routing rules.

`ingress`

* is optional

* Type: `object[]` ([Details](project-defs-ingressdef.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-ingress.md "project.json#/$defs/ProjectSpec/properties/ingress")

### ingress Type

`object[]` ([Details](project-defs-ingressdef.md))

## expireAt

ExpireAt, when set, causes the GC controller to delete the Project
after this time. Useful for ephemeral preview environments.

`expireAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-expireat.md "project.json#/$defs/ProjectSpec/properties/expireAt")

### expireAt Type

`string`

### expireAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")
