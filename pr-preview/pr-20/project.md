# Project Schema

```txt
project.json
```



| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                        |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :---------------------------------------------------------------- |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json](../../schemas/project.json "open original schema") |

## Project Type

unknown ([Project](project.md))

# Project Definitions

## Definitions group Condition

Reference this group by using

```json
{"$ref":"project.json#/$defs/Condition"}
```

| Property                                  | Type     | Required | Nullable       | Defined by                                                                                                                       |
| :---------------------------------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------------------------- |
| [type](#type)                             | `string` | Required | cannot be null | [Project](project-defs-condition-properties-type.md "project.json#/$defs/Condition/properties/type")                             |
| [status](#status)                         | `string` | Required | cannot be null | [Project](project-defs-condition-properties-status.md "project.json#/$defs/Condition/properties/status")                         |
| [lastHeartbeatTime](#lastheartbeattime)   | `string` | Optional | cannot be null | [Project](project-defs-condition-properties-lastheartbeattime.md "project.json#/$defs/Condition/properties/lastHeartbeatTime")   |
| [lastTransitionTime](#lasttransitiontime) | `string` | Optional | cannot be null | [Project](project-defs-condition-properties-lasttransitiontime.md "project.json#/$defs/Condition/properties/lastTransitionTime") |
| [reason](#reason)                         | `string` | Optional | cannot be null | [Project](project-defs-condition-properties-reason.md "project.json#/$defs/Condition/properties/reason")                         |
| [message](#message)                       | `string` | Optional | cannot be null | [Project](project-defs-condition-properties-message.md "project.json#/$defs/Condition/properties/message")                       |

### type

Type is a machine-readable identifier, e.g. "Ready", "Phase".

`type`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-type.md "project.json#/$defs/Condition/properties/type")

#### type Type

`string`

### status

Status is one of True, False, Unknown.

`status`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-status.md "project.json#/$defs/Condition/properties/status")

#### status Type

`string`

### lastHeartbeatTime

LastHeartbeatTime is when this condition was last sampled.

`lastHeartbeatTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-lastheartbeattime.md "project.json#/$defs/Condition/properties/lastHeartbeatTime")

#### lastHeartbeatTime Type

`string`

#### lastHeartbeatTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### lastTransitionTime

LastTransitionTime is when the Status last changed.

`lastTransitionTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-lasttransitiontime.md "project.json#/$defs/Condition/properties/lastTransitionTime")

#### lastTransitionTime Type

`string`

#### lastTransitionTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### reason

Reason is a CamelCase word summarising why the condition has this status.

`reason`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-reason.md "project.json#/$defs/Condition/properties/reason")

#### reason Type

`string`

### message

Message is a human-readable explanation.

`message`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-condition-properties-message.md "project.json#/$defs/Condition/properties/message")

#### message Type

`string`

## Definitions group EnvVar

Reference this group by using

```json
{"$ref":"project.json#/$defs/EnvVar"}
```

| Property        | Type     | Required | Nullable       | Defined by                                                                                       |
| :-------------- | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------- |
| [name](#name)   | `string` | Required | cannot be null | [Project](project-defs-envvar-properties-name.md "project.json#/$defs/EnvVar/properties/name")   |
| [value](#value) | `string` | Optional | cannot be null | [Project](project-defs-envvar-properties-value.md "project.json#/$defs/EnvVar/properties/value") |

### name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-envvar-properties-name.md "project.json#/$defs/EnvVar/properties/name")

#### name Type

`string`

### value



`value`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-envvar-properties-value.md "project.json#/$defs/EnvVar/properties/value")

#### value Type

`string`

## Definitions group IngressAccess

Reference this group by using

```json
{"$ref":"project.json#/$defs/IngressAccess"}
```

| Property        | Type     | Required | Nullable       | Defined by                                                                                                     |
| :-------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------- |
| [scope](#scope) | `string` | Required | cannot be null | [Project](project-defs-ingressaccess-properties-scope.md "project.json#/$defs/IngressAccess/properties/scope") |

### scope

Controls whether a route is exposed to the public internet or only to the Headscale overlay network.

`scope`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressaccess-properties-scope.md "project.json#/$defs/IngressAccess/properties/scope")

#### scope Type

`string`

#### scope Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Internal"` |             |

## Definitions group IngressDef

Reference this group by using

```json
{"$ref":"project.json#/$defs/IngressDef"}
```

| Property          | Type     | Required | Nullable       | Defined by                                                                                                 |
| :---------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------- |
| [name](#name-1)   | `string` | Required | cannot be null | [Project](project-defs-ingressdef-properties-name.md "project.json#/$defs/IngressDef/properties/name")     |
| [host](#host)     | `string` | Optional | cannot be null | [Project](project-defs-ingressdef-properties-host.md "project.json#/$defs/IngressDef/properties/host")     |
| [target](#target) | `object` | Required | cannot be null | [Project](project-defs-ingressdef-properties-target.md "project.json#/$defs/IngressDef/properties/target") |
| [access](#access) | `object` | Optional | cannot be null | [Project](project-defs-ingressaccess.md "project.json#/$defs/IngressDef/properties/access")                |

### name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressdef-properties-name.md "project.json#/$defs/IngressDef/properties/name")

#### name Type

`string`

### host



`host`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingressdef-properties-host.md "project.json#/$defs/IngressDef/properties/host")

#### host Type

`string`

### target

IngressTarget is the backend service and port for an ingress rule.

`target`

* is required

* Type: `object` ([Details](project-defs-ingressdef-properties-target.md))

* cannot be null

* defined in: [Project](project-defs-ingressdef-properties-target.md "project.json#/$defs/IngressDef/properties/target")

#### target Type

`object` ([Details](project-defs-ingressdef-properties-target.md))

### access

IngressAccess defines visibility and auth rules for an ingress endpoint.

`access`

* is optional

* Type: `object` ([Details](project-defs-ingressaccess.md))

* cannot be null

* defined in: [Project](project-defs-ingressaccess.md "project.json#/$defs/IngressDef/properties/access")

#### access Type

`object` ([Details](project-defs-ingressaccess.md))

## Definitions group IngressScope

Reference this group by using

```json
{"$ref":"project.json#/$defs/IngressScope"}
```

| Property | Type | Required | Nullable | Defined by |
| :------- | :--- | :------- | :------- | :--------- |

## Definitions group IngressTarget

Reference this group by using

```json
{"$ref":"project.json#/$defs/IngressTarget"}
```

| Property            | Type      | Required | Nullable       | Defined by                                                                                                         |
| :------------------ | :-------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------- |
| [service](#service) | `string`  | Required | cannot be null | [Project](project-defs-ingresstarget-properties-service.md "project.json#/$defs/IngressTarget/properties/service") |
| [port](#port)       | `integer` | Required | cannot be null | [Project](project-defs-ingresstarget-properties-port.md "project.json#/$defs/IngressTarget/properties/port")       |

### service



`service`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingresstarget-properties-service.md "project.json#/$defs/IngressTarget/properties/service")

#### service Type

`string`

### port



`port`

* is required

* Type: `integer`

* cannot be null

* defined in: [Project](project-defs-ingresstarget-properties-port.md "project.json#/$defs/IngressTarget/properties/port")

#### port Type

`integer`

## Definitions group ObjectMeta

Reference this group by using

```json
{"$ref":"project.json#/$defs/ObjectMeta"}
```

| Property                    | Type     | Required | Nullable       | Defined by                                                                                                           |
| :-------------------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------------- |
| [name](#name-2)             | `string` | Required | cannot be null | [Project](project-defs-objectmeta-properties-name.md "project.json#/$defs/ObjectMeta/properties/name")               |
| [labels](#labels)           | `object` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-labels.md "project.json#/$defs/ObjectMeta/properties/labels")           |
| [annotations](#annotations) | `object` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-annotations.md "project.json#/$defs/ObjectMeta/properties/annotations") |
| [createdAt](#createdat)     | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-createdat.md "project.json#/$defs/ObjectMeta/properties/createdAt")     |
| [updatedAt](#updatedat)     | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-updatedat.md "project.json#/$defs/ObjectMeta/properties/updatedAt")     |

### name

Name is the unique identifier within its Kind namespace.

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-name.md "project.json#/$defs/ObjectMeta/properties/name")

#### name Type

`string`

### labels

Labels are arbitrary key/value pairs used for selection and grouping.

`labels`

* is optional

* Type: `object` ([Details](project-defs-objectmeta-properties-labels.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-labels.md "project.json#/$defs/ObjectMeta/properties/labels")

#### labels Type

`object` ([Details](project-defs-objectmeta-properties-labels.md))

### annotations

Annotations are non-identifying metadata (e.g. human-readable hints).

`annotations`

* is optional

* Type: `object` ([Details](project-defs-objectmeta-properties-annotations.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-annotations.md "project.json#/$defs/ObjectMeta/properties/annotations")

#### annotations Type

`object` ([Details](project-defs-objectmeta-properties-annotations.md))

### createdAt

CreatedAt is set by the server on first write.

`createdAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-createdat.md "project.json#/$defs/ObjectMeta/properties/createdAt")

#### createdAt Type

`string`

#### createdAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### updatedAt

UpdatedAt is set by the server on every write.

`updatedAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-updatedat.md "project.json#/$defs/ObjectMeta/properties/updatedAt")

#### updatedAt Type

`string`

#### updatedAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## Definitions group Project

Reference this group by using

```json
{"$ref":"project.json#/$defs/Project"}
```

| Property                  | Type     | Required | Nullable       | Defined by                                                                                                   |
| :------------------------ | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------- |
| [apiVersion](#apiversion) | `string` | Required | cannot be null | [Project](project-defs-project-properties-apiversion.md "project.json#/$defs/Project/properties/apiVersion") |
| [kind](#kind)             | `string` | Required | cannot be null | [Project](project-defs-project-properties-kind.md "project.json#/$defs/Project/properties/kind")             |
| [metadata](#metadata)     | `object` | Required | cannot be null | [Project](project-defs-objectmeta.md "project.json#/$defs/Project/properties/metadata")                      |
| [spec](#spec)             | `object` | Optional | cannot be null | [Project](project-defs-project-properties-spec.md "project.json#/$defs/Project/properties/spec")             |
| [status](#status-1)       | `object` | Optional | cannot be null | [Project](project-defs-project-properties-status.md "project.json#/$defs/Project/properties/status")         |

### apiVersion

APIVersion identifies the versioned schema this resource conforms to, e.g. "caravanserai/v1".

`apiVersion`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-project-properties-apiversion.md "project.json#/$defs/Project/properties/apiVersion")

#### apiVersion Type

`string`

### kind

Kind is the resource type, e.g. "Node", "Project".

`kind`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-project-properties-kind.md "project.json#/$defs/Project/properties/kind")

#### kind Type

`string`

### metadata

ObjectMeta holds identity and classification metadata common to all resources.

`metadata`

* is required

* Type: `object` ([Details](project-defs-objectmeta.md))

* cannot be null

* defined in: [Project](project-defs-objectmeta.md "project.json#/$defs/Project/properties/metadata")

#### metadata Type

`object` ([Details](project-defs-objectmeta.md))

### spec

ProjectSpec is the desired state declared by the user.

`spec`

* is optional

* Type: `object` ([Details](project-defs-project-properties-spec.md))

* cannot be null

* defined in: [Project](project-defs-project-properties-spec.md "project.json#/$defs/Project/properties/spec")

#### spec Type

`object` ([Details](project-defs-project-properties-spec.md))

### status

ProjectStatus is written by the Controller Manager and Agent.

`status`

* is optional

* Type: `object` ([Details](project-defs-project-properties-status.md))

* cannot be null

* defined in: [Project](project-defs-project-properties-status.md "project.json#/$defs/Project/properties/status")

#### status Type

`object` ([Details](project-defs-project-properties-status.md))

## Definitions group ProjectPhase

Reference this group by using

```json
{"$ref":"project.json#/$defs/ProjectPhase"}
```

| Property | Type | Required | Nullable | Defined by |
| :------- | :--- | :------- | :------- | :--------- |

## Definitions group ProjectSpec

Reference this group by using

```json
{"$ref":"project.json#/$defs/ProjectSpec"}
```

| Property              | Type     | Required | Nullable       | Defined by                                                                                                       |
| :-------------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------------- |
| [services](#services) | `array`  | Required | cannot be null | [Project](project-defs-projectspec-properties-services.md "project.json#/$defs/ProjectSpec/properties/services") |
| [volumes](#volumes)   | `array`  | Optional | cannot be null | [Project](project-defs-projectspec-properties-volumes.md "project.json#/$defs/ProjectSpec/properties/volumes")   |
| [ingress](#ingress)   | `array`  | Optional | cannot be null | [Project](project-defs-projectspec-properties-ingress.md "project.json#/$defs/ProjectSpec/properties/ingress")   |
| [expireAt](#expireat) | `string` | Optional | cannot be null | [Project](project-defs-projectspec-properties-expireat.md "project.json#/$defs/ProjectSpec/properties/expireAt") |

### services

Services is the ordered list of containers to run.

`services`

* is required

* Type: `object[]` ([Details](project-defs-projectspec-properties-services-items.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-services.md "project.json#/$defs/ProjectSpec/properties/services")

#### services Type

`object[]` ([Details](project-defs-projectspec-properties-services-items.md))

### volumes

Volumes are named storage units shared across services.

`volumes`

* is optional

* Type: `object[]` ([Details](project-defs-projectspec-properties-volumes-items.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-volumes.md "project.json#/$defs/ProjectSpec/properties/volumes")

#### volumes Type

`object[]` ([Details](project-defs-projectspec-properties-volumes-items.md))

### ingress

Ingress defines public or internal HTTP routing rules.

`ingress`

* is optional

* Type: `object[]` ([Details](project-defs-ingressdef.md))

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-ingress.md "project.json#/$defs/ProjectSpec/properties/ingress")

#### ingress Type

`object[]` ([Details](project-defs-ingressdef.md))

### expireAt

ExpireAt, when set, causes the GC controller to delete the Project
after this time. Useful for ephemeral preview environments.

`expireAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectspec-properties-expireat.md "project.json#/$defs/ProjectSpec/properties/expireAt")

#### expireAt Type

`string`

#### expireAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## Definitions group ProjectStatus

Reference this group by using

```json
{"$ref":"project.json#/$defs/ProjectStatus"}
```

| Property                  | Type     | Required | Nullable       | Defined by                                                                                                               |
| :------------------------ | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------------- |
| [phase](#phase)           | `string` | Optional | cannot be null | [Project](project-defs-projectstatus-properties-phase.md "project.json#/$defs/ProjectStatus/properties/phase")           |
| [nodeRef](#noderef)       | `string` | Optional | cannot be null | [Project](project-defs-projectstatus-properties-noderef.md "project.json#/$defs/ProjectStatus/properties/nodeRef")       |
| [conditions](#conditions) | `array`  | Optional | cannot be null | [Project](project-defs-projectstatus-properties-conditions.md "project.json#/$defs/ProjectStatus/properties/conditions") |

### phase

Lifecycle state of a Project as maintained by the Controller Manager.

`phase`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectstatus-properties-phase.md "project.json#/$defs/ProjectStatus/properties/phase")

#### phase Type

`string`

#### phase Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value           | Explanation |
| :-------------- | :---------- |
| `"Pending"`     |             |
| `"Scheduled"`   |             |
| `"Running"`     |             |
| `"Failed"`      |             |
| `"Terminating"` |             |
| `"Terminated"`  |             |

### nodeRef

NodeRef is the name of the Node the Scheduler chose. Empty while Pending.

`nodeRef`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-projectstatus-properties-noderef.md "project.json#/$defs/ProjectStatus/properties/nodeRef")

#### nodeRef Type

`string`

### conditions

Conditions is a list of granular observable states.

`conditions`

* is optional

* Type: `object[]` ([Details](project-defs-condition.md))

* cannot be null

* defined in: [Project](project-defs-projectstatus-properties-conditions.md "project.json#/$defs/ProjectStatus/properties/conditions")

#### conditions Type

`object[]` ([Details](project-defs-condition.md))

## Definitions group ServiceDef

Reference this group by using

```json
{"$ref":"project.json#/$defs/ServiceDef"}
```

| Property                      | Type     | Required | Nullable       | Defined by                                                                                                             |
| :---------------------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------------------------- |
| [name](#name-3)               | `string` | Required | cannot be null | [Project](project-defs-servicedef-properties-name.md "project.json#/$defs/ServiceDef/properties/name")                 |
| [image](#image)               | `string` | Required | cannot be null | [Project](project-defs-servicedef-properties-image.md "project.json#/$defs/ServiceDef/properties/image")               |
| [env](#env)                   | `array`  | Optional | cannot be null | [Project](project-defs-servicedef-properties-env.md "project.json#/$defs/ServiceDef/properties/env")                   |
| [volumeMounts](#volumemounts) | `array`  | Optional | cannot be null | [Project](project-defs-servicedef-properties-volumemounts.md "project.json#/$defs/ServiceDef/properties/volumeMounts") |

### name

Name identifies the service within the Project (used as the DNS
hostname inside the shared bridge network).

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-name.md "project.json#/$defs/ServiceDef/properties/name")

#### name Type

`string`

### image

Image is the Docker image reference, e.g. "postgres:15".

`image`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-image.md "project.json#/$defs/ServiceDef/properties/image")

#### image Type

`string`

### env

Env are extra environment variables injected at runtime.

`env`

* is optional

* Type: `object[]` ([Details](project-defs-envvar.md))

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-env.md "project.json#/$defs/ServiceDef/properties/env")

#### env Type

`object[]` ([Details](project-defs-envvar.md))

### volumeMounts

VolumeMounts lists volumes to attach to this container.

`volumeMounts`

* is optional

* Type: `object[]` ([Details](project-defs-servicedef-properties-volumemounts-items.md))

* cannot be null

* defined in: [Project](project-defs-servicedef-properties-volumemounts.md "project.json#/$defs/ServiceDef/properties/volumeMounts")

#### volumeMounts Type

`object[]` ([Details](project-defs-servicedef-properties-volumemounts-items.md))

## Definitions group VolumeDef

Reference this group by using

```json
{"$ref":"project.json#/$defs/VolumeDef"}
```

| Property        | Type     | Required | Nullable       | Defined by                                                                                           |
| :-------------- | :------- | :------- | :------------- | :--------------------------------------------------------------------------------------------------- |
| [name](#name-4) | `string` | Required | cannot be null | [Project](project-defs-volumedef-properties-name.md "project.json#/$defs/VolumeDef/properties/name") |
| [type](#type-1) | `string` | Required | cannot be null | [Project](project-defs-volumedef-properties-type.md "project.json#/$defs/VolumeDef/properties/type") |

### name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumedef-properties-name.md "project.json#/$defs/VolumeDef/properties/name")

#### name Type

`string`

### type

Governs the lifecycle and backup behaviour of a Volume.

`type`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumedef-properties-type.md "project.json#/$defs/VolumeDef/properties/type")

#### type Type

`string`

#### type Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value         | Explanation |
| :------------ | :---------- |
| `"Ephemeral"` |             |

## Definitions group VolumeMount

Reference this group by using

```json
{"$ref":"project.json#/$defs/VolumeMount"}
```

| Property                | Type     | Required | Nullable       | Defined by                                                                                                         |
| :---------------------- | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------- |
| [name](#name-5)         | `string` | Required | cannot be null | [Project](project-defs-volumemount-properties-name.md "project.json#/$defs/VolumeMount/properties/name")           |
| [mountPath](#mountpath) | `string` | Required | cannot be null | [Project](project-defs-volumemount-properties-mountpath.md "project.json#/$defs/VolumeMount/properties/mountPath") |

### name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumemount-properties-name.md "project.json#/$defs/VolumeMount/properties/name")

#### name Type

`string`

### mountPath



`mountPath`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-volumemount-properties-mountpath.md "project.json#/$defs/VolumeMount/properties/mountPath")

#### mountPath Type

`string`

## Definitions group VolumeType

Reference this group by using

```json
{"$ref":"project.json#/$defs/VolumeType"}
```

| Property | Type | Required | Nullable | Defined by |
| :------- | :--- | :------- | :------- | :--------- |
